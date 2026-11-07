package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	pairingLife     = 60 * time.Second
	maxPairings     = 32
	pairingsPerCode = 4
	sessionIdle     = 120 * time.Second
	pairFirstSize   = 48

	// Failed Pair attempts allowed before a failure answers 429, per source and in total.
	triesPerAddress = 5
	triesInAll      = 30
	triesWindow     = time.Minute
	// Codes tried a second, past which nothing more is tried: about a tenth of a core.
	triesCeiling = 40_000
	// Requests a second one session can make. A real console makes a few.
	requestsPerSecond = 50
)

// LinkHandler is what the console port needs from the links: an existing link's requests, and a
// new server using a link code.
type LinkHandler interface {
	Handle(link, op string, raw []byte) any
	Accept(code store.Code, key []byte, raw []byte) (any, error)
}

// peerKind is who a session belongs to.
type peerKind int

const (
	peerConsole peerKind = iota
	peerServer
)

// withinBudget counts one request against this second's budget. It holds the session's lock.
func (s *session) withinBudget(now time.Time) bool {
	if second := now.Unix(); second != s.second {
		s.second, s.count = second, 0
	}
	s.count++
	return s.count <= requestsPerSecond
}

type session struct {
	second  int64
	count   int
	s       *proof.Session
	kind    peerKind
	console int
	link    string
	key     string
	last    time.Time
	mu      sync.Mutex
}

type pairing struct {
	mu      sync.Mutex
	r       *proof.Responder
	code    string
	started time.Time
	msg3    []byte // what was answered, so an identical retry gets the identical bytes
	reply   []byte
}

// Handler moves sealed bytes: everything a console or a linked server sends comes through here,
// as POST /linkup/v1. Every other request is answered as if nothing were here.
type Handler struct {
	hub   *Hub
	store *store.Store
	log   *logfile.Log
	key   proof.KeyPair
	links LinkHandler
	now   func() time.Time

	mu        sync.Mutex
	pairings  map[string]*pairing
	sessions  map[[proof.SessionIDSize]byte]*session
	pending   map[string][proof.SessionIDSize]byte // unconfirmed session per key
	confirmed map[string][proof.SessionIDSize]byte // the session in use per key
	tries     map[string][]time.Time
	allTries  []time.Time
	highestT  map[string]int64

	ceilingSecond int64
	ceilingCount  int
}

// NewHandler makes the console port's handler.
func NewHandler(hub *Hub, st *store.Store, log *logfile.Log, key proof.KeyPair, links LinkHandler) *Handler {
	return &Handler{
		hub: hub, store: st, log: log, key: key, links: links, now: time.Now,
		pairings: map[string]*pairing{}, sessions: map[[proof.SessionIDSize]byte]*session{},
		pending: map[string][proof.SessionIDSize]byte{}, confirmed: map[string][proof.SessionIDSize]byte{},
		tries: map[string][]time.Time{}, highestT: map[string]int64{},
	}
}

// ServeHTTP is the whole port. A stranger can tell it is a LinkUp server from a 403, and little more.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != proof.Path {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, proof.MaxBody+1))
	if err != nil || len(body) < 2 || len(body) > proof.MaxBody {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch body[0] {
	case proof.KindPairFirst:
		h.pairFirst(w, r, body[1:])
	case proof.KindPairSecond:
		h.pairSecond(w, r, body[1:])
	case proof.KindOpen:
		h.open(w, body[1:])
	case proof.KindSealed:
		h.sealed(w, r, body[1:])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var carrierGrade = netip.MustParsePrefix("100.64.0.0/10")

// sourceKey is what a failed Pair counts against: a public address, or a whole /64 on IPv6. A private
// source, which is what a reverse proxy looks like, counts only in the total.
func sourceKey(source string) string {
	addr, err := netip.ParseAddr(source)
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || carrierGrade.Contains(addr) {
		return ""
	}
	if addr.Is6() {
		p, err := addr.Prefix(64)
		if err != nil {
			return ""
		}
		return p.String()
	}
	return addr.String()
}

// tooManyFailed notes one failed Pair and says whether its answer becomes 429. The limit only ever
// changes that answer: a message that fits a code always goes through, so a stranger cannot lock a
// real code out. Sources stay in memory only.
func (h *Handler) tooManyFailed(source string) bool {
	key := sourceKey(source)
	h.log.PairFailed()
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	cutoff := now.Add(-triesWindow)
	h.allTries = append(recent(h.allTries, cutoff), now)
	for s, list := range h.tries {
		if list = recent(list, cutoff); len(list) == 0 {
			delete(h.tries, s)
		} else {
			h.tries[s] = list
		}
	}
	if key != "" {
		h.tries[key] = append(h.tries[key], now)
	}
	return len(h.allTries) > triesInAll || (key != "" && len(h.tries[key]) > triesPerAddress)
}

func recent(list []time.Time, cutoff time.Time) []time.Time {
	kept := list[:0]
	for _, at := range list {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// overOwnLimit says whether a public source has already failed too often. It is refused before any
// work, and does not count toward the ceiling, so one host cannot use the ceiling up for everyone.
func (h *Handler) overOwnLimit(source string) bool {
	key := sourceKey(source)
	if key == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tries[key] = recent(h.tries[key], h.now().Add(-triesWindow))
	return len(h.tries[key]) >= triesPerAddress
}

// underCeiling holds the work of trying messages down, in codes tried, whatever else is happening.
func (h *Handler) underCeiling(codes int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now().Unix()
	if now != h.ceilingSecond {
		h.ceilingSecond, h.ceilingCount = now, 0
	}
	h.ceilingCount += max(codes, 1)
	return h.ceilingCount <= triesCeiling
}

// pairFirst is message 1: the joiner's proof, checked against every waiting code.
func (h *Handler) pairFirst(w http.ResponseWriter, r *http.Request, msg []byte) {
	if len(msg) != pairFirstSize {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if h.overOwnLimit(sourceOf(r)) {
		h.log.PairFailed()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	st := h.store.Snapshot()
	now := h.now()
	var ks [][]byte
	var ids []string
	for _, c := range st.Codes {
		if now.Before(c.Expires) {
			ks = append(ks, c.K)
			ids = append(ids, c.ID)
		}
	}
	if !h.underCeiling(len(ks)) {
		h.log.PairFailed()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	responder, err := proof.TryFirst(ks, h.key, msg)
	if err != nil {
		if h.tooManyFailed(sourceOf(r)) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	hello, _ := json.Marshal(proof.Hello{Protocol: proof.Protocol, Oldest: proof.Oldest, Name: st.Settings.Name})
	second, err := responder.SecondMessage(hello)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	idBytes := make([]byte, proof.PairIDSize)
	if _, err := rand.Read(idBytes); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	code := ids[responder.Index]
	h.mu.Lock()
	h.makeRoomLocked(code)
	h.pairings[string(idBytes)] = &pairing{r: responder, code: code, started: now}
	h.mu.Unlock()
	writeBytes(w, append(idBytes, second...))
}

// makeRoomLocked keeps pairings in progress to their time and their number, oldest first. A replay of
// someone's message 1 can push out only that code's own pairings, not anyone else's.
func (h *Handler) makeRoomLocked(code string) {
	now := h.now()
	for id, p := range h.pairings {
		if now.Sub(p.started) > pairingLife {
			delete(h.pairings, id)
		}
	}
	h.dropOldestLocked(pairingsPerCode, func(p *pairing) bool { return p.code == code })
	h.dropOldestLocked(maxPairings, func(*pairing) bool { return true })
}

func (h *Handler) dropOldestLocked(limit int, counted func(*pairing) bool) {
	count := 0
	var oldestID string
	var oldest time.Time
	for id, p := range h.pairings {
		if !counted(p) {
			continue
		}
		count++
		if oldestID == "" || p.started.Before(oldest) {
			oldestID, oldest = id, p.started
		}
	}
	if count >= limit && oldestID != "" {
		delete(h.pairings, oldestID)
	}
}

func writeBytes(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

// consoleJoining is what a console says in message 3.
type consoleJoining struct {
	Kind     string `json:"kind"`
	Protocol int    `json:"protocol"`
	Oldest   int    `json:"oldest"`
	Stream   int    `json:"stream"`
	Account  string `json:"account"`
	Username string `json:"username"`
}

type joinResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Console int    `json:"console,omitempty"`
}

// pairSecond is message 3: the joiner's key and who they are. The code is used up here, under the
// store's lock, so the first to finish takes the place. The answer is kept for the rest of the
// pairing's time: the server cannot know it arrived, and an identical retry gets the same bytes,
// because sealing again would use nonce 0 twice.
func (h *Handler) pairSecond(w http.ResponseWriter, r *http.Request, rest []byte) {
	if len(rest) <= proof.PairIDSize {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, msg3 := string(rest[:proof.PairIDSize]), rest[proof.PairIDSize:]
	h.mu.Lock()
	p := h.pairings[id]
	h.mu.Unlock()
	if p == nil || h.now().Sub(p.started) > pairingLife {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reply != nil {
		if bytes.Equal(p.msg3, msg3) {
			writeBytes(w, p.reply)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	joining, err := p.r.Third(msg3)
	if err != nil {
		// A failed read leaves the handshake unusable, so this pairing is over.
		h.mu.Lock()
		delete(h.pairings, id)
		h.mu.Unlock()
		if h.tooManyFailed(sourceOf(r)) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	json.Unmarshal(joining, &kind)
	var reply any
	switch kind.Kind {
	case "console":
		reply = h.joinConsole(p, joining)
	case "server":
		reply = h.joinServer(p, joining)
	default:
		reply = joinResult{Error: "wrong_code"}
	}
	plain, err := json.Marshal(reply)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sealed, err := p.r.SealReply(plain)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.msg3, p.reply = bytes.Clone(msg3), sealed
	writeBytes(w, sealed)
}

// joinConsole is a console using a code. A reinstall keeps the console's number and people, with a
// new key. A new console gets a number. The account signed in at this moment is the friend.
func (h *Hub) joinConsole(c store.Code, key []byte, j consoleJoining) (joinResult, error) {
	if !cleanWord(j.Account, maxAccount) || !cleanWord(j.Username, maxUsername) {
		return joinResult{Error: "bad_request"}, ErrBadRequest
	}
	number := 0
	var person, friend string
	err := h.store.Update(func(st *store.State) error {
		if st.Code(c.ID) == nil {
			return ErrUnavailable
		}
		if st.LinkByKey(key) != nil {
			return ErrUnavailable
		}
		known := st.ConsoleByKey(key)
		switch c.Kind {
		case store.CodeConsole:
			con := st.ConsoleByNumber(c.Console)
			if con == nil || (known != nil && known.Number != con.Number) {
				return ErrUnavailable
			}
			con.Key, con.Removed = key, false
			number = con.Number
		case store.CodeOwn, store.CodeInvite:
			switch {
			case known == nil:
				number = st.NextConsole
				st.NextConsole++
				st.Consoles = append(st.Consoles, store.Console{Number: number, Key: key, Paired: h.now()})
			case known.Removed:
				known.Removed = false
				number = known.Number
			default:
				// A key already here has friends added on the owner's page, with no new code.
				return ErrUnavailable
			}
		default:
			return ErrUnavailable
		}
		p := st.PersonByAccount(number, j.Account)
		if p == nil {
			st.People = append(st.People, store.Person{ID: store.NewID(), Console: number, Account: j.Account, Username: j.Username, LastSeen: h.now()})
			p = &st.People[len(st.People)-1]
		} else {
			p.Username, p.LastSeen = j.Username, h.now()
		}
		person = p.ID
		if c.Kind == store.CodeInvite && c.For != "" && st.Person(c.For) != nil && c.For != p.ID {
			friend = c.For
			st.AddFriends(p.ID, friend)
			if c.Note != "" && p.Note == "" {
				p.Note = c.Note
			}
		}
		st.RemoveCode(c.ID)
		return nil
	})
	if err != nil {
		return joinResult{Error: "unavailable"}, err
	}
	h.log.ConsolePaired(number)
	h.FriendsChanged(person, friend)
	h.mu.Lock()
	links := h.links
	h.mu.Unlock()
	links.Changed()
	return joinResult{OK: true, Console: number}, nil
}

func (h *Handler) joinConsole(p *pairing, raw []byte) joinResult {
	var j consoleJoining
	if json.Unmarshal(raw, &j) != nil {
		return joinResult{Error: "bad_request"}
	}
	st := h.store.Snapshot()
	code := st.Code(p.code)
	if code == nil || code.Kind == store.CodeLink || !h.now().Before(code.Expires) {
		return joinResult{Error: "wrong_code"}
	}
	result, err := h.hub.joinConsole(*code, p.r.Key, j)
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	if code.Kind == store.CodeConsole {
		// A reinstall: whatever the old key had open is finished.
		h.DropConsole(result.Console)
	}
	return result
}

// joinServer is another server using a link code. A key that is a console here, or this server's own,
// is refused with the code kept. Otherwise the code goes as soon as it is used, whatever the link makes
// of it, so it cannot be tried twice.
func (h *Handler) joinServer(p *pairing, raw []byte) any {
	if bytes.Equal(p.r.Key, h.key.Public) {
		return joinResult{Error: "wrong_code"}
	}
	var c store.Code
	err := h.store.Update(func(st *store.State) error {
		code := st.Code(p.code)
		if code == nil || code.Kind != store.CodeLink || !h.now().Before(code.Expires) || st.ConsoleByKey(p.r.Key) != nil {
			return ErrUnavailable
		}
		c = *code
		st.RemoveCode(p.code)
		return nil
	})
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	reply, err := h.links.Accept(c, p.r.Key, raw)
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	return reply
}

// open is the first message of a session. A key this server does not know gets what anything else
// gets. A key it removed gets a real answer, inside the handshake, so a fake server cannot say it.
func (h *Handler) open(w http.ResponseWriter, msg []byte) {
	accepting, key, payload, err := proof.Accept(h.key, msg)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var said proof.Hello
	if json.Unmarshal(payload, &said) != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	st := h.store.Snapshot()
	theirs := proof.Hello{Protocol: proof.Protocol, Oldest: proof.Oldest}
	s := &session{key: string(key), last: h.now()}
	removed := false
	if c := st.ConsoleByKey(key); c != nil {
		removed = c.Removed
		s.kind, s.console = peerConsole, c.Number
	} else if l := st.LinkByKey(key); l != nil {
		s.kind, s.link = peerServer, l.ID
	} else {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// A recorded Open is dropped here, before anything is kept for it. Only a known key gets this
	// far, so a stranger cannot fill the table with made-up keys.
	h.mu.Lock()
	replayed := said.T <= h.highestT[string(key)]
	if !replayed {
		h.highestT[string(key)] = said.T
	}
	h.mu.Unlock()
	if replayed {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if removed {
		theirs.Removed = true
		plain, _ := json.Marshal(theirs)
		answer, _, err := accepting.Answer(plain)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeBytes(w, answer)
		return
	}

	var id [proof.SessionIDSize]byte
	if _, err := rand.Read(id[:]); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	theirs.Session = hex.EncodeToString(id[:])
	plain, _ := json.Marshal(theirs)
	answer, sess, err := accepting.Answer(plain)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sess.ID = id
	s.s = sess

	h.mu.Lock()
	h.expireLocked()
	if old, ok := h.pending[s.key]; ok {
		delete(h.sessions, old)
	}
	h.pending[s.key] = id
	h.sessions[id] = s
	h.mu.Unlock()

	if s.kind == peerConsole {
		h.hub.Connected(s.console, key, said.Stream)
	}
	writeBytes(w, answer)
}

// expireLocked drops sessions nobody has used for a while.
func (h *Handler) expireLocked() {
	now := h.now()
	for id, s := range h.sessions {
		if now.Sub(s.last) > sessionIdle {
			delete(h.sessions, id)
			if h.pending[s.key] == id {
				delete(h.pending, s.key)
			}
			if h.confirmed[s.key] == id {
				delete(h.confirmed, s.key)
			}
		}
	}
}

// sealed is every request after that. A new session replaces the old only once its first request
// arrives, so a replayed first message cannot push anyone off.
func (h *Handler) sealed(w http.ResponseWriter, r *http.Request, body []byte) {
	id, counter, ciphertext, ok := proof.ParseSealed(body)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Under the lock, so two copies sent at once get one answer: the counter is marked before the work.
	h.mu.Lock()
	idle := h.now().Sub(s.last) > sessionIdle
	if idle {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	if idle {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	plain, err := s.s.OpenRequest(counter, ciphertext)
	busy := err == nil && !s.withinBudget(h.now())
	s.mu.Unlock()
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if busy {
		s.mu.Lock()
		sealed := s.s.SealAnswer(counter, []byte(`{"error":"busy"}`))
		s.mu.Unlock()
		writeBytes(w, sealed)
		return
	}
	h.mu.Lock()
	s.last = h.now()
	if h.pending[s.key] == id {
		delete(h.pending, s.key)
		if old, ok := h.confirmed[s.key]; ok && old != id {
			delete(h.sessions, old)
		}
		h.confirmed[s.key] = id
	}
	h.mu.Unlock()

	var answer any
	if s.kind == peerConsole {
		answer = h.operate(r.Context(), s.console, plain)
	} else {
		var op struct {
			Op string `json:"op"`
		}
		json.Unmarshal(plain, &op)
		answer = h.links.Handle(s.link, op.Op, plain)
	}
	out, err := json.Marshal(answer)
	if err != nil {
		out = []byte(`{"error":"internal"}`)
	}
	s.mu.Lock()
	sealed := s.s.SealAnswer(counter, out)
	s.mu.Unlock()
	writeBytes(w, sealed)
}

// DropConsole closes a console's sessions, after it was removed or left.
func (h *Handler) DropConsole(number int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		if s.kind == peerConsole && s.console == number {
			delete(h.sessions, id)
			delete(h.pending, s.key)
			delete(h.confirmed, s.key)
		}
	}
}

// DropLink closes a link's sessions, after it was removed.
func (h *Handler) DropLink(link string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		if s.kind == peerServer && s.link == link {
			delete(h.sessions, id)
			delete(h.pending, s.key)
			delete(h.confirmed, s.key)
		}
	}
}

// request is every field an operation can have. Each op reads what it needs.
type request struct {
	Op       string `json:"op"`
	Account  string `json:"account"`
	Username string `json:"username"`
	Status   string `json:"status"`
	After    int    `json:"after"`
	Friend   string `json:"friend"`
	Sealed   string `json:"sealed"`
	Invite   string `json:"invite"`
	Session  string `json:"session"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
}

var errRemoved = errors.New("removed")

type errorAnswer struct {
	Error string `json:"error"`
}

func failed(err error) errorAnswer {
	return errorAnswer{Error: err.Error()}
}

func okOr(err error) any {
	if err != nil {
		return failed(err)
	}
	return struct{}{}
}

// operate does one request from a console.
func (h *Handler) operate(ctx context.Context, number int, plain []byte) any {
	var q request
	if json.Unmarshal(plain, &q) != nil {
		return failed(ErrBadRequest)
	}
	removed := false
	h.store.View(func(st *store.State) {
		c := st.ConsoleByNumber(number)
		removed = c == nil || c.Removed
	})
	if removed && q.Op != "wait" {
		return failed(errRemoved)
	}
	h.hub.Seen(number)
	switch q.Op {
	case "signin":
		id, err := h.hub.Signin(number, q.Account, q.Username, q.Status)
		if err != nil {
			return failed(err)
		}
		return struct {
			Person string `json:"person"`
		}{id}
	case "signout":
		h.hub.Signout(number)
		return struct{}{}
	case "status":
		return okOr(h.hub.SetStatus(number, q.Status))
	case "friends":
		list, err := h.hub.Friends(number)
		if err != nil {
			return failed(err)
		}
		return struct {
			Friends []Friend `json:"friends"`
		}{list}
	case "config":
		st := h.store.Snapshot()
		stun := []string{}
		for _, x := range st.Settings.Stun {
			stun = append(stun, x.Address)
		}
		return struct {
			Name string   `json:"name"`
			Stun []string `json:"stun"`
		}{st.Settings.Name, stun}
	case "wait":
		epoch, events, next := h.hub.Wait(ctx, number, q.After)
		return struct {
			Epoch  string  `json:"epoch"`
			Events []Event `json:"events"`
			Next   int     `json:"next"`
		}{epoch, events, next}
	case "invite":
		id, err := h.hub.Invite(number, q.Invite, q.Friend, q.Sealed)
		if err != nil {
			return failed(err)
		}
		return struct {
			Invite    string `json:"invite"`
			ExpiresIn int    `json:"expires_in"`
		}{id, int(inviteLife.Seconds())}
	case "cancel":
		return okOr(h.hub.Cancel(number, q.Invite))
	case "decline":
		return okOr(h.hub.Decline(number, q.Invite))
	case "join":
		id, err := h.hub.Join(number, q.Invite)
		if err != nil {
			return failed(err)
		}
		return struct {
			Session string `json:"session"`
		}{id}
	case "signal":
		return okOr(h.hub.Signal(number, q.Session, q.Sealed))
	case "end":
		return okOr(h.hub.End(number, q.Session))
	case "report":
		return okOr(h.hub.Report(number, q.Session, q.Path, q.Reason))
	case "relay":
		creds, why, err := h.hub.Relay(number, q.Session)
		if err != nil {
			return failed(err)
		}
		return struct {
			Relay  any    `json:"relay"`
			Reason string `json:"reason,omitempty"`
		}{relayOrNil(creds), why}
	case "leave":
		err := h.hub.Leave(number)
		h.DropConsole(number)
		return okOr(err)
	}
	return failed(errors.New("unknown"))
}

func relayOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

// EncodeBox writes a sealed box as the operations carry it.
func EncodeBox(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeBox reads a sealed box as the operations carry it.
func DecodeBox(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
