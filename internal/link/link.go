// Package link is server to server. A link connects chosen pairs of people, and only they are
// ever mentioned across it: everyone else on each server stays on that server, and there is no
// chain to a third. It uses the same pairing and the same sessions as a console, on the same port,
// and each side sends to the other by its own short requests.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MMagTech/cabinet-linkup/internal/console"
	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	helloEvery = 60 * time.Second
	// A server has to miss this many in a row before it reads as out of reach.
	missesToFail = 2
	// A pair a linked server has stopped mentioning is gone, unless it is newer than this: the
	// two servers may not have heard of it at the same moment.
	pairGrace = 5 * time.Minute
	// How many messages can wait to go to one server. A server this far behind is out of reach.
	queueLength = 256
	// How often a told address is tried.
	candidateEvery = 10 * time.Minute

	maxName = 20
)

// State is how a link is doing right now.
type State struct {
	Reachable    bool
	BehindTheirs bool
	BehindOurs   bool
}

// ourHello is what one server says to another every minute and when something changes.
type ourHello struct {
	Op      string     `json:"op"`
	Name    string     `json:"name"`
	Address string     `json:"address"`
	Pairs   []pairLine `json:"pairs"`
}

// pairLine names one pair by both servers' ids. Ours is the sender's person, theirs the receiver's.
type pairLine struct {
	Ours     string `json:"ours"`
	Theirs   string `json:"theirs"`
	Status   string `json:"status"`
	Username string `json:"username"`
	Key      []byte `json:"key"`
	Stream   int    `json:"stream"`
}

type message struct {
	Op      string `json:"op"`
	Invite  string `json:"invite,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Sealed  string `json:"sealed,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Session string `json:"session,omitempty"`
	Ours    string `json:"ours,omitempty"`
	Theirs  string `json:"theirs,omitempty"`
}

type answer struct {
	Error  string             `json:"error,omitempty"`
	Relay  *relay.Credentials `json:"relay,omitempty"`
	Reason string             `json:"reason,omitempty"`
}

// peer is what is kept in memory about one linked server.
type peer struct {
	client *proof.Client
	queue  chan message
	done   chan struct{}
	misses int
	state  State
	// heard is what that server last said of its people. It shows only while this server reaches
	// theirs, since a knock goes from here to there.
	heard map[string]string
}

// Manager holds the links, and speaks for this server to the others.
type Manager struct {
	store  *store.Store
	log    *logfile.Log
	hub    *console.Hub
	key    proof.KeyPair
	client func(address string, peer []byte) *proof.Client
	// OnRemoved is told when a link goes, so its sessions close.
	OnRemoved func(link string)

	mu    sync.Mutex
	peers map[string]*peer
	kick  chan struct{}
	ctx   context.Context
}

// New makes a manager. Nothing is sent until Run.
func New(st *store.Store, log *logfile.Log, hub *console.Hub, key proof.KeyPair) *Manager {
	m := &Manager{store: st, log: log, hub: hub, key: key, peers: map[string]*peer{}, kick: make(chan struct{}, 1), ctx: context.Background()}
	m.client = func(address string, peerKey []byte) *proof.Client { return proof.NewClient(address, key, peerKey) }
	return m
}

// Run keeps every link up to date until the context ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	t := time.NewTicker(helloEvery)
	defer t.Stop()
	m.helloAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
			// A burst of changes becomes one message.
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		m.helloAll()
	}
}

// Changed says this server's name, address or people changed, so linked servers hear now.
func (m *Manager) Changed() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// State says how a link is doing.
func (m *Manager) State(id string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peers[id]; p != nil {
		return p.state
	}
	var failing bool
	m.store.View(func(st *store.State) {
		if l := st.Link(id); l != nil {
			failing = !l.Failing.IsZero() || l.Address == ""
		}
	})
	return State{Reachable: !failing}
}

// peerFor finds or makes what is kept about a link.
func (m *Manager) peerFor(l store.Link) *peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[l.ID]
	if p == nil {
		p = &peer{client: m.client(l.Address, l.Key), queue: make(chan message, queueLength), done: make(chan struct{}), state: State{Reachable: l.Failing.IsZero() && l.Address != ""}}
		m.peers[l.ID] = p
		go m.send(l.ID, p, m.ctx)
	} else {
		p.client.SetAddress(l.Address)
	}
	p.client.SetDialCheck(dialCheck(l.Typed || l.ProvedPrivate))
	return p
}

// dialCheck holds a told address to the public internet, unless it was shown to be private when it
// was proved. An address the owner typed is free.
func dialCheck(allowPrivate bool) func(net.IP) error {
	return func(ip net.IP) error {
		if !allowPrivate && relay.Blocked(ip) {
			return errors.New("that address is private")
		}
		return nil
	}
}

// send takes messages off one link's queue in order, so connection details arrive as sent.
func (m *Manager) send(id string, p *peer, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case msg := <-p.queue:
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			var reply answer
			p.client.Call(call, msg, &reply)
			cancel()
		}
	}
}

func (m *Manager) enqueue(id string, msg message) {
	var l *store.Link
	st := m.store.Snapshot()
	if l = st.Link(id); l == nil {
		return
	}
	p := m.peerFor(*l)
	select {
	case p.queue <- msg:
	default:
	}
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peers[id]; p != nil {
		close(p.done)
		delete(m.peers, id)
	}
}

// helloAll sends each linked server what is true now. It is also how a link notices it is down.
func (m *Manager) helloAll() {
	st := m.store.Snapshot()
	var wg sync.WaitGroup
	for _, l := range st.Links {
		wg.Add(1)
		go func(l store.Link) {
			defer wg.Done()
			m.hello(&st, l)
		}(l)
	}
	wg.Wait()
}

func (m *Manager) helloFor(st *store.State, l store.Link) ourHello {
	h := ourHello{Op: "link_hello", Name: st.Settings.Name, Address: st.Settings.Address, Pairs: []pairLine{}}
	for _, pair := range l.Pairs {
		p := st.Person(pair.Ours)
		if p == nil {
			continue
		}
		at := m.hub.KeyConsole(pair.Ours)
		line := pairLine{Ours: pair.Ours, Theirs: pair.Theirs, Username: p.Username, Status: m.hub.StatusOf(pair.Ours), Stream: m.hub.StreamOf(at)}
		if c := st.ConsoleByNumber(at); c != nil {
			line.Key = c.Key
		}
		h.Pairs = append(h.Pairs, line)
	}
	return h
}

func (m *Manager) hello(st *store.State, l store.Link) {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	// A link removed since the snapshot was taken stays removed.
	gone := false
	m.store.View(func(now *store.State) { gone = now.Link(l.ID) == nil })
	if gone {
		return
	}
	if l.Candidate != "" && time.Since(l.CandidateTried) >= candidateEvery {
		m.startCandidate(ctx, l)
	}
	p := m.peerFor(l)
	if l.Address == "" {
		m.noteResult(l.ID, p, errors.New("no address proved yet"))
		return
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var reply answer
	err := p.client.Call(call, m.helloFor(st, l), &reply)
	m.noteResult(l.ID, p, err)
}

// startCandidate is the only way a told address is ever saved: an Open under the remembered key.
// Something on a home network cannot finish that handshake, so it is never adopted. It holds up
// nothing, and the time between tries belongs to the link, however often the address changes, so a
// linked server cannot use it to find out what is on this server's network.
func (m *Manager) startCandidate(ctx context.Context, l store.Link) {
	begun := false
	now := time.Now()
	m.store.Change(func(st *store.State) bool {
		current := st.Link(l.ID)
		if current == nil || current.Candidate != l.Candidate || now.Sub(current.CandidateTried) < candidateEvery {
			return false
		}
		current.CandidateTried = now
		begun = true
		return true
	})
	if !begun {
		return
	}
	go func() {
		probe := m.client(l.Candidate, l.Key)
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if probe.Prove(call) != nil {
			return
		}
		dialed := probe.Dialed()
		adopted := false
		m.store.Change(func(st *store.State) bool {
			current := st.Link(l.ID)
			if current == nil || current.Candidate != l.Candidate {
				return false
			}
			current.Address, current.Candidate = current.Candidate, ""
			current.Typed = false
			current.ProvedPrivate = dialed != nil && relay.Blocked(dialed)
			adopted = true
			return true
		})
		if adopted {
			m.Changed()
		}
	}()
}

// noteResult records whether a server answered, and what it stated.
func (m *Manager) noteResult(id string, p *peer, err error) {
	theirs := p.client.Peer()
	m.mu.Lock()
	changedUp, changedDown := false, false
	if err != nil {
		p.misses++
		if p.misses >= missesToFail && p.state.Reachable {
			p.state.Reachable = false
			changedDown = true
		}
	} else {
		p.misses = 0
		if !p.state.Reachable {
			p.state.Reachable = true
			changedUp = true
		}
		p.state.BehindTheirs = theirs.Protocol < proof.Oldest
		p.state.BehindOurs = proof.Protocol < theirs.Oldest
	}
	behind := p.state.BehindTheirs || p.state.BehindOurs
	reachable := p.state.Reachable
	heard := p.heard
	m.mu.Unlock()

	number := 0
	if changedUp || changedDown {
		now := time.Now()
		m.store.Update(func(st *store.State) error {
			if l := st.Link(id); l != nil {
				number = l.Number
				if changedDown {
					l.Failing = now
				} else {
					l.Failing = time.Time{}
				}
			}
			return nil
		})
	}
	if changedDown {
		m.log.LinkDown(number)
	}
	if changedUp {
		m.log.LinkUp(number)
	}
	switch {
	case !reachable:
		m.hub.RemoteStatus(id, behind, nil)
	case changedUp:
		m.hub.RemoteStatus(id, behind, heard)
	}
}

// ---- What the admin page asks ----

// joinHello is what a server sends with a link code: who it is, and the account picked.
type joinHello struct {
	Kind     string  `json:"kind"`
	Protocol int     `json:"protocol"`
	Oldest   int     `json:"oldest"`
	Name     string  `json:"name"`
	Address  string  `json:"address"`
	Account  account `json:"account"`
}

type account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Key      []byte `json:"key"`
	Stream   int    `json:"stream"`
}

type joinReply struct {
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
	Name    string  `json:"name"`
	Account account `json:"account"`
}

func (m *Manager) accountOf(st *store.State, id string) (account, bool) {
	p := st.Person(id)
	if p == nil {
		return account{}, false
	}
	at := m.hub.KeyConsole(id)
	a := account{ID: id, Username: p.Username, Stream: m.hub.StreamOf(at)}
	if c := st.ConsoleByNumber(at); c != nil {
		a.Key = c.Key
	}
	return a, true
}

// Enter uses a code from another server. Those two accounts are friends from then on.
func (m *Manager) Enter(address, code, person string) error {
	tidy, ok := proof.TidyCode(code)
	if !ok {
		return store.ErrWrongCode
	}
	st := m.store.Snapshot()
	mine, ok := m.accountOf(&st, person)
	if !ok {
		return store.ErrWrongCode
	}
	hello := joinHello{Kind: "server", Protocol: proof.Protocol, Oldest: proof.Oldest, Name: st.Settings.Name, Address: st.Settings.Address, Account: mine}
	c := m.client(address, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var reply joinReply
	key, err := c.Pair(ctx, tidy, hello, &reply)
	switch {
	case errors.Is(err, proof.ErrWrongCode), errors.Is(err, proof.ErrTooManyTries):
		return store.ErrWrongCode
	case err != nil:
		return store.ErrCantReach
	case !reply.OK:
		return store.ErrWrongCode
	}
	if !cleanLine(reply.Name, maxName) || !goodAccount(reply.Account) {
		return store.ErrWrongCode
	}
	return m.addPair(key, reply.Name, person, reply.Account, func(l *store.Link) {
		// The owner typed this one, so it is free, and it just worked.
		l.Address, l.Typed, l.Candidate = address, true, ""
	})
}

// Accept is the other half of Enter: another server used this server's code. It returns what to
// send back, or an error if the code is not for this.
func (m *Manager) Accept(code store.Code, key []byte, raw []byte) (any, error) {
	var hello joinHello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Kind != "server" {
		return nil, console.ErrBadRequest
	}
	if slices.Equal(key, m.key.Public) {
		return nil, console.ErrBadRequest
	}
	if !cleanLine(hello.Name, maxName) || !proof.PushedAddressOK(hello.Address) || !goodAccount(hello.Account) {
		return nil, console.ErrBadRequest
	}
	st := m.store.Snapshot()
	mine, ok := m.accountOf(&st, code.For)
	if !ok {
		return nil, console.ErrBadRequest
	}
	err := m.addPair(key, hello.Name, code.For, hello.Account, func(l *store.Link) {
		// Told, so it is only a candidate until it proves itself.
		if l.Address != hello.Address {
			l.Candidate = hello.Address
		}
	})
	if err != nil {
		return nil, err
	}
	return joinReply{OK: true, Name: st.Settings.Name, Account: mine}, nil
}

// addPair puts a pair on the link to that server, making the link if this is the first.
func (m *Manager) addPair(key []byte, name, ours string, theirs account, address func(*store.Link)) error {
	added := 0
	touched := false
	err := m.store.Update(func(st *store.State) error {
		l := st.LinkByKey(key)
		if l == nil {
			st.Links = append(st.Links, store.Link{ID: store.NewID(), Number: st.NextLink, Key: key})
			st.NextLink++
			l = &st.Links[len(st.Links)-1]
			added = l.Number
		}
		l.Name = name
		address(l)
		if !slices.ContainsFunc(l.Pairs, func(p store.Pair) bool { return p.Ours == ours && p.Theirs == theirs.ID }) {
			l.Pairs = append(l.Pairs, store.Pair{ID: store.NewID(), Ours: ours, Theirs: theirs.ID, TheirName: theirs.Username, TheirKey: theirs.Key, TheirStream: theirs.Stream, Added: time.Now()})
			touched = true
		}
		return nil
	})
	if err != nil {
		m.log.Problem(logfile.ProblemSave)
		return err
	}
	if added != 0 {
		m.log.LinkAdded(added)
	}
	if touched {
		m.hub.FriendsChanged(ours)
	}
	m.Changed()
	return nil
}

// RemovePair ends one friendship across a link, on both servers.
func (m *Manager) RemovePair(id, pair string) error {
	var ours, theirs string
	err := m.store.Update(func(st *store.State) error {
		l := st.Link(id)
		if l == nil {
			return nil
		}
		i := slices.IndexFunc(l.Pairs, func(p store.Pair) bool { return p.ID == pair })
		if i < 0 {
			return nil
		}
		ours, theirs = l.Pairs[i].Ours, l.Pairs[i].Theirs
		l.Pairs = slices.Delete(l.Pairs, i, i+1)
		return nil
	})
	if err != nil || ours == "" {
		return err
	}
	m.hub.EndPair(id, ours, theirs)
	m.hub.FriendsChanged(ours)
	m.enqueue(id, message{Op: "link_pair_remove", Ours: ours, Theirs: theirs})
	return nil
}

// Unlink removes the whole link, on both servers.
func (m *Manager) Unlink(id string) error {
	st := m.store.Snapshot()
	l := st.Link(id)
	if l == nil {
		return nil
	}
	p := m.peerFor(*l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var reply answer
	p.client.Call(ctx, message{Op: "link_unlink"}, &reply)
	cancel()
	return m.removeLink(id)
}

func (m *Manager) removeLink(id string) error {
	number := 0
	var people []string
	err := m.store.Update(func(st *store.State) error {
		l := st.Link(id)
		if l == nil {
			return nil
		}
		number = l.Number
		for _, p := range l.Pairs {
			people = append(people, p.Ours)
		}
		st.Links = slices.DeleteFunc(st.Links, func(x store.Link) bool { return x.ID == id })
		return nil
	})
	if err != nil {
		return err
	}
	m.hub.EndLink(id)
	m.forget(id)
	if m.OnRemoved != nil {
		m.OnRemoved(id)
	}
	if number != 0 {
		m.log.LinkRemoved(number)
	}
	m.hub.FriendsChanged(people...)
	return nil
}

// ---- What the hub asks ----

// Invite passes an invite to the other server, and waits for its answer: it may refuse. For someone
// on more than one console a hello goes first, with the key of the console the invite comes from:
// the friend's console opens the box with the key its server has.
func (m *Manager) Invite(id, invite, from, to, sealed string) error {
	st := m.store.Snapshot()
	if l, p := st.Link(id), st.Person(from); l != nil && p != nil && len(p.Others) > 0 {
		m.hello(&st, *l)
	}
	var reply answer
	if err := m.callOn(id, message{Op: "link_invite", Invite: invite, From: from, To: to, Sealed: sealed}, &reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return console.ErrUnavailable
	}
	return nil
}

// InviteEnded tells the other server an invite ended.
func (m *Manager) InviteEnded(id, invite, reason string) {
	m.enqueue(id, message{Op: "link_invite_ended", Invite: invite, Reason: reason})
}

// Join asks the inviter's server. It decides, because a join and a cancel can cross.
func (m *Manager) Join(id, invite string) error {
	var reply answer
	if err := m.callOn(id, message{Op: "link_join", Invite: invite}, &reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return console.ErrUnavailable
	}
	return nil
}

// Signal passes sealed connection details on, in order.
func (m *Manager) Signal(id, session, sealed string) {
	m.enqueue(id, message{Op: "link_signal", Session: session, Sealed: sealed})
}

// End tells the other server a session ended.
func (m *Manager) End(id, session string) {
	m.enqueue(id, message{Op: "link_end", Session: session})
}

// Relay asks the other server for a way through its relay.
func (m *Manager) Relay(id, session string) (*relay.Credentials, string) {
	var reply answer
	if err := m.callOn(id, message{Op: "link_relay", Session: session}, &reply); err != nil {
		return nil, "off"
	}
	if reply.Relay != nil {
		for _, u := range reply.Relay.URLs {
			if !strings.HasPrefix(u, "turn:") {
				return nil, "off"
			}
		}
	}
	return reply.Relay, reply.Reason
}

func (m *Manager) callOn(id string, msg message, reply *answer) error {
	st := m.store.Snapshot()
	l := st.Link(id)
	if l == nil {
		return console.ErrUnavailable
	}
	p := m.peerFor(*l)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.client.Call(ctx, msg, reply)
}

// ---- What another server says ----

// Handle answers one request from a linked server. It only ever acts on that link's own pairs.
func (m *Manager) Handle(id, op string, raw []byte) any {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return answer{Error: "bad_request"}
	}
	switch op {
	case "link_hello":
		var h ourHello
		if err := json.Unmarshal(raw, &h); err != nil {
			return answer{Error: "bad_request"}
		}
		m.heard(id, h)
	case "link_pair_remove":
		m.heardPairRemoved(id, msg.Theirs, msg.Ours)
	case "link_unlink":
		m.removeLink(id)
	case "link_invite":
		if err := m.hub.RemoteInvite(id, msg.Invite, msg.From, msg.To, msg.Sealed); err != nil {
			return answer{Error: "unavailable"}
		}
	case "link_invite_ended":
		m.hub.RemoteInviteEnded(id, msg.Invite, msg.Reason)
	case "link_join":
		if err := m.hub.RemoteJoin(id, msg.Invite); err != nil {
			return answer{Error: "unavailable"}
		}
	case "link_signal":
		m.hub.RemoteSignal(id, msg.Session, msg.Sealed)
	case "link_end":
		m.hub.RemoteEnd(id, msg.Session)
	case "link_relay":
		creds, why := m.hub.RemoteRelay(id, msg.Session)
		return answer{Relay: creds, Reason: why}
	default:
		return answer{Error: "unknown"}
	}
	return answer{}
}

// goodAccount holds what another server says about one of its people to what one really is.
func goodAccount(a account) bool {
	return cleanLine(a.ID, 64) && cleanUsername(a.Username) && len(a.Key) == proof.KeySize
}

// cleanUsername holds a username to what a console may send: 128 bytes, one line.
func cleanUsername(s string) bool {
	return len(s) <= 128 && cleanLine(s, 128)
}

// cleanLine holds a name or an address from another server to what a person typed: one short line.
func cleanLine(s string, limit int) bool {
	if s == "" || utf8.RuneCountInString(s) > limit || !utf8.ValidString(s) {
		return false
	}
	return !containsControl(s)
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// heard saves what a linked server said: its name and address, and its people in a pair. Anyone
// not in a pair is dropped, and a pair it has stopped mentioning is gone.
func (m *Manager) heard(id string, h ourHello) {
	statuses := map[string]string{}
	var touched []string
	var dropped []store.Pair
	var current store.Link
	now := time.Now()
	// Saved only if something saved changed: status lives in memory, and this comes every minute.
	err := m.store.Change(func(st *store.State) bool {
		l := st.Link(id)
		if l == nil {
			return false
		}
		changed := false
		if cleanLine(h.Name, maxName) && l.Name != h.Name {
			l.Name, changed = h.Name, true
		}
		if proof.PushedAddressOK(h.Address) && h.Address != l.Address && h.Address != l.Candidate {
			l.Candidate, changed = h.Address, true
			m.Changed()
		}
		kept := l.Pairs[:0]
		for _, pair := range l.Pairs {
			found := false
			for _, line := range h.Pairs {
				if line.Ours != pair.Theirs || line.Theirs != pair.Ours {
					continue
				}
				found = true
				statuses[pair.Theirs] = line.Status
				if cleanUsername(line.Username) && pair.TheirName != line.Username {
					pair.TheirName, changed = line.Username, true
					touched = append(touched, pair.Ours)
				}
				if len(line.Key) == proof.KeySize && !slices.Equal(pair.TheirKey, line.Key) {
					pair.TheirKey, changed = line.Key, true
					touched = append(touched, pair.Ours)
				}
				if pair.TheirStream != line.Stream {
					pair.TheirStream, changed = line.Stream, true
					touched = append(touched, pair.Ours)
				}
			}
			if found || now.Sub(pair.Added) < pairGrace {
				kept = append(kept, pair)
			} else {
				dropped = append(dropped, pair)
				touched = append(touched, pair.Ours)
				changed = true
			}
		}
		l.Pairs = kept
		current = *l
		return changed
	})
	if err != nil {
		m.log.Problem(logfile.ProblemSave)
	}
	if current.ID == "" {
		return
	}
	for _, pair := range dropped {
		m.hub.EndPair(id, pair.Ours, pair.Theirs)
	}
	// Their server reaching this one says nothing of this one reaching theirs, and a knock goes that
	// way: until it does, their people are Offline here (PROTOCOL.md, "Operations").
	p := m.peerFor(current)
	m.mu.Lock()
	behind := p.state.BehindTheirs || p.state.BehindOurs
	p.heard = statuses
	if !p.state.Reachable {
		statuses = nil
	}
	m.mu.Unlock()
	m.hub.RemoteStatus(id, behind, statuses)
	m.hub.FriendsChanged(touched...)
}

func (m *Manager) heardPairRemoved(id, ours, theirs string) {
	removed := false
	err := m.store.Update(func(st *store.State) error {
		l := st.Link(id)
		if l == nil {
			return nil
		}
		before := len(l.Pairs)
		l.Pairs = slices.DeleteFunc(l.Pairs, func(p store.Pair) bool { return p.Ours == ours && p.Theirs == theirs })
		removed = len(l.Pairs) != before
		return nil
	})
	if err == nil && removed {
		m.hub.EndPair(id, ours, theirs)
		m.hub.FriendsChanged(ours)
	}
}

// DisplayAddress is what the admin page shows for a link: the proved address, or else what it was told.
func DisplayAddress(l store.Link) string {
	if l.Address != "" {
		return l.Address
	}
	return l.Candidate
}
