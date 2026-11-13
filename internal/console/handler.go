package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	sessionIdle = 120 * time.Second
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
	swept     time.Time // when sources with no recent failure were last dropped
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

func writeBytes(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
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
	Chosen   bool   `json:"chosen"`
	Hosts    *bool  `json:"hosts"`
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
		id, chose, err := h.hub.Signin(number, q.Account, q.Username, q.Status, joinsOnly(q.Hosts))
		if err != nil {
			return failed(err)
		}
		chosen := StatusOnline
		if chose {
			chosen = StatusOffline
		}
		return struct {
			Person string `json:"person"`
			Chosen string `json:"chosen"`
		}{id, chosen}
	case "signout":
		h.hub.Signout(number)
		return struct{}{}
	case "status":
		return okOr(h.hub.SetStatus(number, q.Status, q.Chosen))
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
