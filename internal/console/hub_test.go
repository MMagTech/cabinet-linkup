package console

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

type fakeRelay struct {
	why   string
	ended []string
	games int
}

func (f *fakeRelay) Available() string { return f.why }
func (f *fakeRelay) Credentials(session string) (relay.Credentials, error) {
	return relay.Credentials{Username: "u." + session, Password: "p"}, nil
}
func (f *fakeRelay) EndSession(s string) { f.ended = append(f.ended, s) }
func (f *fakeRelay) AddGame()            { f.games++ }

type hubRig struct {
	t     *testing.T
	h     *Hub
	st    *store.Store
	relay *fakeRelay
	log   *logfile.Log
	clock time.Time
	mu    sync.Mutex
}

func newHubRig(t *testing.T) *hubRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, func(*store.State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	log, err := logfile.Open(filepath.Join(dir, "log.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &hubRig{t: t, st: st, relay: &fakeRelay{}, log: log, clock: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	r.h = NewHub(st, log, r.relay)
	r.h.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }
	return r
}

func (r *hubRig) advance(d time.Duration) {
	r.mu.Lock()
	r.clock = r.clock.Add(d)
	r.mu.Unlock()
}

// console adds a paired console and signs one account in, Online.
func (r *hubRig) console(number int, username string) string {
	r.t.Helper()
	r.st.Update(func(st *store.State) error {
		if st.ConsoleByNumber(number) == nil {
			st.Consoles = append(st.Consoles, store.Console{Number: number, Key: []byte{byte(number)}})
		}
		return nil
	})
	r.h.Connected(number, []byte{byte(number)}, 1)
	id, _, err := r.h.Signin(number, username, username, StatusOnline, false)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *hubRig) friends(a, b string) {
	r.st.Update(func(st *store.State) error { st.AddFriends(a, b); return nil })
}

func (r *hubRig) events(number int) []Event {
	r.t.Helper()
	l := r.h.lives[number]
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	out := append([]Event(nil), l.events...)
	l.events = nil
	return out
}

func newInviteID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hasEvent(events []Event, kind, reason string) bool {
	for _, e := range events {
		if e.Type == kind && e.Reason == reason {
			return true
		}
	}
	return false
}

func statusOf(t *testing.T, h *Hub, number int, friend string) string {
	t.Helper()
	list, err := h.Friends(number)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.ID == friend {
			return f.Status
		}
	}
	t.Fatalf("%s is not on the list", friend)
	return ""
}

type fakeLinker struct {
	mu        sync.Mutex
	invited   []string
	ended     []string
	joined    []string
	signals   []string
	finish    []string
	joinErr   error
	inviteErr error
	creds     *relay.Credentials
	why       string
}

func (f *fakeLinker) Changed() {}
func (f *fakeLinker) Invite(link, invite, from, to, sealed string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invited = append(f.invited, to)
	return f.inviteErr
}
func (f *fakeLinker) InviteEnded(link, invite, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, reason)
}
func (f *fakeLinker) Join(link, invite string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = append(f.joined, invite)
	return f.joinErr
}
func (f *fakeLinker) Signal(link, session, sealed string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sealed)
}
func (f *fakeLinker) End(link, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finish = append(f.finish, session)
}
func (f *fakeLinker) Relay(link, session string) (*relay.Credentials, string) {
	return f.creds, f.why
}
func (f *fakeLinker) count(list *[]string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(*list)
}

func linkedRig(t *testing.T) (*hubRig, *fakeLinker, string, string) {
	r := newHubRig(t)
	maya := r.console(1, "maya")
	other := r.console(2, "other")
	r.st.Update(func(st *store.State) error {
		st.Links = []store.Link{{ID: "l1", Number: 1, Name: "Riley's", Pairs: []store.Pair{{ID: "p1", Ours: maya, Theirs: "june-there", TheirName: "june", TheirKey: []byte{9}, TheirStream: 1}}}}
		return nil
	})
	f := &fakeLinker{}
	r.h.SetLinker(f)
	return r, f, maya, other
}
