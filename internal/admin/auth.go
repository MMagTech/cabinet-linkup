package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	cookieName  = "linkup_session"
	sessionLife = 30 * 24 * time.Hour
	maxSessions = 16

	// Passwords are at least this long: the data folder holds a hash someone could try
	// guesses against if they ever copied it.
	minPassword = 8

	// Sign-in tries: this many wrong in this long and it waits.
	signinTries  = 5
	signinWindow = time.Minute

	passwordTime   = 3
	passwordMemory = 64 * 1024
	passwordLanes  = 1
	passwordLength = 32
)

// hashing allows one slow hash at a time, so a burst of sign-ins cannot use up a small
// device's memory.
var hashing sync.Mutex

// finishHash gives a slow hash's memory back, so a small device does not keep 64 MiB for one sign-in.
func finishHash() {
	debug.FreeOSMemory()
	hashing.Unlock()
}

// hashPassword stores a password as Argon2id in its usual text form.
func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	hashing.Lock()
	defer finishHash()
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordLanes, passwordLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, passwordMemory, passwordTime, passwordLanes,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// checkPassword compares a password with a stored hash, with the parameters the hash was made
// with, so that raising them later does not lock anyone out.
func checkPassword(stored, password string) bool {
	fields := strings.Split(stored, "$")
	if len(fields) != 6 || fields[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(want) == 0 {
		return false
	}
	hashing.Lock()
	defer finishHash()
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// session is one signed-in browser. Sessions stay in memory, so a restart signs everyone out.
type session struct {
	token   string
	expires time.Time
	flash   *Notice
}

// sessions holds who is signed in and how many wrong passwords came lately.
type sessions struct {
	mu    sync.Mutex
	byID  map[string]*session
	wrong []time.Time
	nowFn func() time.Time
}

func newSessions() *sessions {
	return &sessions{byID: map[string]*session{}, nowFn: time.Now}
}

func (s *sessions) start(w http.ResponseWriter) *session {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	now := s.nowFn()
	se := &session{token: hex.EncodeToString(raw), expires: now.Add(sessionLife)}
	s.mu.Lock()
	for token, old := range s.byID {
		if now.After(old.expires) {
			delete(s.byID, token)
		}
	}
	for len(s.byID) >= maxSessions {
		var oldest *session
		for _, old := range s.byID {
			if oldest == nil || old.expires.Before(oldest.expires) {
				oldest = old
			}
		}
		delete(s.byID, oldest.token)
	}
	s.byID[se.token] = se
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: se.token, Path: "/", MaxAge: int(sessionLife.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return se
}

func (s *sessions) get(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.byID[c.Value]
	if se == nil || s.nowFn().After(se.expires) {
		return nil
	}
	return se
}

func (s *sessions) end(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.byID, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// endOthers signs out every other browser, after a password change.
func (s *sessions) endOthers(keep *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token := range s.byID {
		if keep == nil || token != keep.token {
			delete(s.byID, token)
		}
	}
}

func (s *sessions) setFlash(se *session, n *Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	se.flash = n
}

func (s *sessions) takeFlash(se *session) *Notice {
	if se == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := se.flash
	se.flash = nil
	return n
}

// tooManyWrong says whether sign-ins are waiting after too many wrong passwords. It is checked
// before the slow hash, so a flood of guesses costs nothing.
func (s *sessions) tooManyWrong() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.nowFn().Add(-signinWindow)
	kept := s.wrong[:0]
	for _, at := range s.wrong {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	s.wrong = kept
	return len(s.wrong) >= signinTries
}

func (s *sessions) countWrong() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wrong = append(s.wrong, s.nowFn())
}
