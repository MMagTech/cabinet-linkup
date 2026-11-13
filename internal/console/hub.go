// Package console is what consoles talk to: pairing, presence, invites and connection details,
// one long request at a time. handler.go moves sealed bytes; this file is the people.
//
// The server owns everything about people: each account's status, who can be invited, refusing
// a knock on anyone Offline or Linked up, the invite's 30 minutes, declines, ending a second
// invite. The console owns the TV and the game. So the hub says what happened and never what to
// display. All of it stays in memory: a restart clears who is online and every invite.
package console

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

// The waits. Both invite times are easy to change after feedback.
const (
	inviteLife = 30 * time.Minute
	// A console with no request for this long shows Offline.
	consoleSilence = 75 * time.Second
	// How long the one long request is held.
	waitHold = 25 * time.Second
	// How often a person's last-seen day is written, at most.
	seenEvery = time.Hour
	// A session ends on the server when a console here has been silent this long, and at the second
	// time regardless, so a lost message or a vanished console cannot leave anyone stuck as Linked up.
	sessionSilence = 5 * time.Minute
	longestSession = 24 * time.Hour
	// Sealed boxes past this in one list are left for the event that already carried them.
	maxFriendsBytes = 192 * 1024

	maxEvents        = 256
	maxInviteBox     = 2 * 1024
	maxSignalBox     = 16 * 1024
	maxWaitBytes     = 64 * 1024
	knocksAllowed    = 10
	knocksWindow     = 10 * time.Minute
	maxUsername      = 128
	maxAccount       = 128
	peoplePerConsole = 32
	tickEvery        = 5 * time.Second
)

// The three things a person can be.
const (
	StatusOnline   = "online"
	StatusLinkedUp = "linkedup"
	StatusOffline  = "offline"
)

// Why an invite ended.
const (
	EndCancelled   = "cancelled"
	EndTimeout     = "timeout"
	EndDeclined    = "declined"
	EndUnavailable = "unavailable"
)

// The words an operation can fail with.
var (
	ErrUnavailable = errors.New("unavailable")
	ErrNotYours    = errors.New("not_yours")
	ErrNoAccount   = errors.New("no_account")
	ErrBadRequest  = errors.New("bad_request")
)

// Event is one thing a console hears about, on its one long request.
type Event struct {
	Seq     int    `json:"seq"`
	Type    string `json:"type"`
	Invite  string `json:"invite,omitempty"`
	From    string `json:"from,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Session string `json:"session,omitempty"`
	Sealed  string `json:"sealed,omitempty"`
	Status  string `json:"status,omitempty"`
}

// Friend is one line of a person's list. The owner's notes are never in it.
type Friend struct {
	ID       string        `json:"id"`
	Username string        `json:"username"`
	Server   string        `json:"server"`
	Status   string        `json:"status"`
	Key      []byte        `json:"key"`
	Stream   int           `json:"stream"`
	Invite   *FriendInvite `json:"invite,omitempty"`
}

// FriendInvite is an invite from this friend, waiting for an answer.
type FriendInvite struct {
	ID     string `json:"id"`
	Sealed string `json:"sealed"`
}

// Linker is what the hub needs from the links to other servers. Each call goes out to the one
// server the pair belongs to, and only ever mentions that pair.
type Linker interface {
	Changed()
	Invite(link, invite, fromOurs, toTheirs, sealed string) error
	InviteEnded(link, invite, reason string)
	Join(link, invite string) error
	Signal(link, session, sealed string)
	End(link, session string)
	Relay(link, session string) (*relay.Credentials, string)
}

// RelayService is what the hub needs from the relay.
type RelayService interface {
	Available() string
	Credentials(session string) (relay.Credentials, error)
	EndSession(session string)
	AddGame()
}

// live is one console that is connected now.
type live struct {
	number   int
	key      []byte
	lastSeen time.Time
	waiting  int
	person   string
	status   string
	stream   int
	events   []Event
	next     int
	heard    int  // the last event it said it has
	silent   bool // it has gone quiet, and its friends have heard
	// When its person last showed up here: a sign-in, or a status that is not Offline after one
	// that was. Of a person's consoles showing them Online, the latest is where a knock rings.
	activeAt time.Time
	wake     chan struct{}
}

// invite is one invite, and then the session it became. Either end can be on a linked server,
// and then its person is an id on that server and link says which server. Its id is the same on
// both servers, made by the inviter's. Each end on this server is on one console for the whole of
// it, because the boxes are sealed for that console's key.
type invite struct {
	id                     string
	from, to               string
	fromConsole, toConsole int // 0 for an end on a linked server
	link                   string
	fromHere               bool // the inviter is on this server, and this server decides
	sealed                 string
	expires                time.Time
	joined                 bool
	joinedAt               time.Time
	relayed                bool // this server's relay gave out a password for it
	reported               bool
}

// consoleOf is the console a person on this server is in an invite on, or 0 if they are not in it here.
func (inv *invite) consoleOf(person string) int {
	switch {
	case inv.fromHere && inv.from == person:
		return inv.fromConsole
	case inv.to == person && (inv.link == "" || !inv.fromHere):
		return inv.toConsole
	}
	return 0
}

// remote is what a linked server last said about one of its people.
type remote struct {
	status   string
	username string
	key      []byte
	stream   int
}

// Hub is who is on, and every invite.
type Hub struct {
	store *store.Store
	log   *logfile.Log
	relay RelayService
	epoch string
	now   func() time.Time

	mu      sync.Mutex
	links   Linker
	lives   map[int]*live
	invites map[string]*invite
	remotes map[string]map[string]*remote // link, then their person id
	behind  map[string]bool               // links that have fallen behind
	knocks  map[string][]time.Time        // recent knocks, from one person to another
}

// NewHub makes an empty hub.
func NewHub(st *store.Store, log *logfile.Log, r RelayService) *Hub {
	return &Hub{
		store: st, log: log, relay: r, epoch: store.NewID(), now: time.Now,
		links: noLinks{}, lives: map[int]*live{}, invites: map[string]*invite{},
		remotes: map[string]map[string]*remote{}, behind: map[string]bool{}, knocks: map[string][]time.Time{},
	}
}

// SetLinker gives the hub its way to other servers.
func (h *Hub) SetLinker(l Linker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.links = l
}

// Run ends what has run out of time, until the context ends.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Tick()
			h.store.Flush()
			h.log.FlushCounts()
		}
	}
}

// Tick notices consoles that have gone silent and invites past their time.
func (h *Hub) Tick() {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.store.Snapshot()
	touched := map[string]bool{}
	for _, l := range h.lives {
		if l.person != "" && !h.liveLocked(l, now) && !l.silent {
			// The account's own status stays as it said: a console that comes back after a blip
			// should not have to know it was gone.
			l.silent = true
			touched[l.person] = true
			for _, inv := range h.invites {
				if inv.fromHere && inv.from == l.person && inv.fromConsole == l.number && !inv.joined {
					h.endInviteLocked(inv, EndCancelled, false)
				}
			}
		}
	}
	for _, inv := range h.invites {
		switch {
		case !inv.joined && now.After(inv.expires):
			if inv.fromHere {
				h.endInviteLocked(inv, EndTimeout, true)
				touched[inv.from] = true
			} else {
				// The inviter's server says this too. A linked server that has gone away must not
				// leave a knock on someone's list, so this server's own clock ends it as well.
				delete(h.invites, inv.id)
				h.pushOn(inv, inv.to, Event{Type: "invite_ended", Invite: inv.id, Reason: EndTimeout})
				h.pushTo(&st, inv.to, Event{Type: "friends"})
			}
		case inv.joined && (now.Sub(inv.joinedAt) > longestSession || h.silentLocked(inv, now)):
			h.quietEndLocked(&st, inv)
		}
	}
	for id := range touched {
		h.changedLocked(&st, id)
	}
}

// silentLocked says whether a session's console on this server has gone quiet. The game may still be
// running between the consoles, and that does not need the server.
func (h *Hub) silentLocked(inv *invite, now time.Time) bool {
	for _, n := range []int{inv.fromConsole, inv.toConsole} {
		if n == 0 {
			continue
		}
		l := h.lives[n]
		if l == nil || (l.waiting == 0 && now.Sub(l.lastSeen) > sessionSilence) {
			return true
		}
	}
	return false
}

// quietEndLocked ends a session on the server with no word to the consoles: its game does not go
// through the server and carries on, but its relay stops and both people can be knocked on again.
func (h *Hub) quietEndLocked(st *store.State, inv *invite) {
	delete(h.invites, inv.id)
	h.relay.EndSession(inv.id)
	h.changedLocked(st, inv.from, inv.to)
}

// liveLocked says whether a console counts as on: it asked recently, or is asking now.
func (h *Hub) liveLocked(l *live, now time.Time) bool {
	return l.waiting > 0 || now.Sub(l.lastSeen) < consoleSilence
}

// Connected notes a console opening a session, with the versions it stated.
func (h *Hub) Connected(number int, key []byte, stream int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, key)
	l.stream = stream
	l.lastSeen = h.now()
}

func (h *Hub) liveFor(number int, key []byte) *live {
	l := h.lives[number]
	if l == nil {
		l = &live{number: number, key: key, status: StatusOffline, wake: make(chan struct{}, 1)}
		h.lives[number] = l
	}
	if key != nil {
		l.key = key
	}
	return l
}

// Seen notes any request from a console.
func (h *Hub) Seen(number int) {
	h.mu.Lock()
	l := h.liveFor(number, nil)
	l.lastSeen = h.now()
	person := l.person
	back := l.silent
	l.silent = false
	h.mu.Unlock()
	// After the lock: a save must not hold up everyone else's request.
	h.touchUsed(number)
	if person == "" {
		return
	}
	h.touchSeen(person)
	if back {
		st := h.store.Snapshot()
		h.mu.Lock()
		h.changedLocked(&st, person)
		h.mu.Unlock()
	}
}

func (h *Hub) touchSeen(person string) {
	now := h.now()
	h.store.Touch(func(st *store.State) bool {
		p := st.Person(person)
		if p == nil || now.Sub(p.LastSeen) < seenEvery {
			return false
		}
		p.LastSeen = now
		return true
	})
}

// showingLocked says whether a console is on with this person as its current one, and not Offline.
func (h *Hub) showingLocked(n int, id string) (*live, bool) {
	l := h.lives[n]
	if l == nil || l.person != id || !h.liveLocked(l, h.now()) || l.status == StatusOffline {
		return nil, false
	}
	return l, true
}

// touchUsed notes when a console last asked anything, for the owner's page.
func (h *Hub) touchUsed(number int) {
	now := h.now()
	h.store.Touch(func(st *store.State) bool {
		c := st.ConsoleByNumber(number)
		if c == nil || now.Sub(c.LastUsed) < seenEvery {
			return false
		}
		c.LastUsed = now
		return true
	})
}

// statusLocked is what a person on this server is right now: Offline if they chose it, otherwise
// the most of what their consoles say.
func (h *Hub) statusLocked(st *store.State, id string) string {
	p := st.Person(id)
	if p == nil || p.ChoseOffline {
		return StatusOffline
	}
	on := false
	for _, n := range p.Consoles() {
		l, ok := h.showingLocked(n, id)
		if !ok {
			continue
		}
		if l.status == StatusLinkedUp {
			return StatusLinkedUp
		}
		on = true
	}
	if !on {
		return StatusOffline
	}
	for _, inv := range h.invites {
		if (inv.fromHere && inv.from == id) || (inv.joined && inv.link == "" && inv.to == id) || (inv.joined && !inv.fromHere && inv.to == id) {
			return StatusLinkedUp
		}
	}
	return StatusOnline
}

// Online says whether a person shows as on, for the admin page.
func (h *Hub) Online(id string) bool {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked(&st, id) != StatusOffline
}

// StatusOf is what a person is right now, for linked servers to hear.
func (h *Hub) StatusOf(id string) string {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked(&st, id)
}

// keyConsoleLocked is the console whose key a person's friends are given, and where a knock on them
// rings. While they are in an invite or a session it is that one's console, because its boxes are
// sealed for that key. Otherwise it is the one they showed up on last of those showing them Online,
// or with none, the one they used last.
func (h *Hub) keyConsoleLocked(p *store.Person) int {
	invited := 0
	for _, inv := range h.invites {
		n := inv.consoleOf(p.ID)
		if n == 0 {
			continue
		}
		if inv.joined {
			return n
		}
		invited = n
	}
	if invited != 0 {
		return invited
	}
	best, at := p.Console, time.Time{}
	for _, n := range p.Consoles() {
		if l, ok := h.showingLocked(n, p.ID); ok && (at.IsZero() || l.activeAt.After(at)) {
			best, at = n, l.activeAt
		}
	}
	return best
}

// KeyConsole is the console whose key a person's friends are given, for linked servers to hear.
func (h *Hub) KeyConsole(id string) int {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	p := st.Person(id)
	if p == nil {
		return 0
	}
	return h.keyConsoleLocked(p)
}

// StreamOf is the stream version a console last stated.
func (h *Hub) StreamOf(number int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.lives[number]; l != nil {
		return l.stream
	}
	return 0
}

// push adds an event to a console's queue and wakes its long request. "The list changed" is said
// once until it has been heard.
func (l *live) push(e Event) {
	if e.Type == "friends" && len(l.events) > 0 {
		if last := l.events[len(l.events)-1]; last.Type == "friends" && last.Seq > l.heard {
			return
		}
	}
	l.next++
	e.Seq = l.next
	l.events = append(l.events, e)
	if len(l.events) > maxEvents {
		l.events = slices.Delete(l.events, 0, len(l.events)-maxEvents)
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// pushTo adds an event for a person, on each of their consoles that is listening with them as its
// current one.
func (h *Hub) pushTo(st *store.State, person string, e Event) {
	p := st.Person(person)
	if p == nil {
		return
	}
	for _, n := range p.Consoles() {
		if l := h.lives[n]; l != nil && l.person == person {
			l.push(e)
		}
	}
}

// pushOn adds an event about an invite or a session for one person in it, on the console they are
// in it on, if they are still its current one.
func (h *Hub) pushOn(inv *invite, person string, e Event) {
	if l := h.lives[inv.consoleOf(person)]; l != nil && l.person == person {
		l.push(e)
	}
}

// changedLocked tells everyone who can see a person that their list changed, and linked servers.
func (h *Hub) changedLocked(st *store.State, ids ...string) {
	for _, id := range ids {
		for _, f := range st.FriendsOf(id) {
			h.pushTo(st, f, Event{Type: "friends"})
		}
	}
	go h.links.Changed()
}

// FriendsChanged tells these people's consoles that their lists changed.
func (h *Hub) FriendsChanged(people ...string) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range people {
		h.pushTo(&st, id, Event{Type: "friends"})
	}
}

// ConsoleRemoved tells a console it was taken off the server, and ends what it was part of. Its
// people's invites and sessions on other consoles carry on.
func (h *Hub) ConsoleRemoved(number int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.lives[number]; l != nil {
		l.person = ""
		l.status = StatusOffline
		l.push(Event{Type: "removed"})
	}
	h.endConsoleLocked(number)
}

// EndBetween ends what two people on this server have open, after their friendship was removed.
func (h *Hub) EndBetween(a, b string) {
	h.endMatching(func(inv *invite) bool {
		return inv.link == "" && ((inv.from == a && inv.to == b) || (inv.from == b && inv.to == a))
	})
}

// EndPair ends what one pair on a link has open, after that pair was removed.
func (h *Hub) EndPair(link, ours, theirs string) {
	h.endMatching(func(inv *invite) bool {
		return inv.link == link && ((inv.from == ours && inv.to == theirs) || (inv.from == theirs && inv.to == ours))
	})
}

// EndLink ends everything on a link, after it was removed.
func (h *Hub) EndLink(link string) {
	h.endMatching(func(inv *invite) bool { return inv.link == link })
}

// endMatching ends invites and sessions on the owner's word. A knock just goes; a game in progress
// carries on between the consoles, but its relay stops.
func (h *Hub) endMatching(match func(*invite) bool) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, inv := range h.invites {
		if !match(inv) {
			continue
		}
		if inv.joined {
			h.quietEndLocked(&st, inv)
			continue
		}
		delete(h.invites, inv.id)
		h.relay.EndSession(inv.id)
		for _, id := range []string{inv.from, inv.to} {
			h.pushOn(inv, id, Event{Type: "invite_ended", Invite: inv.id, Reason: EndUnavailable})
		}
		h.changedLocked(&st, inv.from, inv.to)
	}
}

// endConsoleLocked ends every invite and session on a console. A knock from it is taken back, and
// one ringing it is unavailable, which its inviter hears.
func (h *Hub) endConsoleLocked(number int) {
	st := h.store.Snapshot()
	for _, inv := range h.invites {
		switch {
		case inv.fromConsole != number && inv.toConsole != number:
		case inv.joined:
			h.quietEndLocked(&st, inv)
		case inv.fromConsole == number:
			h.endInviteLocked(inv, EndCancelled, false)
		case inv.fromHere:
			h.endInviteLocked(inv, EndUnavailable, true)
		default:
			delete(h.invites, inv.id)
			h.links.InviteEnded(inv.link, inv.id, EndUnavailable)
		}
	}
}

// ---- What a console asks ----

func cleanWord(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && !containsControl(s)
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// goodInviteID is the shape the inviter's id must have: 16 bytes as hex.
func goodInviteID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// knockAllowedLocked counts one knock from one person to another, and refuses too many, so a hostile
// friend cannot keep a TV ringing.
func (h *Hub) knockAllowedLocked(from, to string) bool {
	key := from + "|" + to
	now := h.now()
	cutoff := now.Add(-knocksWindow)
	kept := h.knocks[key][:0]
	for _, at := range h.knocks[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= knocksAllowed {
		h.knocks[key] = kept
		return false
	}
	h.knocks[key] = append(kept, now)
	return true
}

func cleanStatus(s string) bool {
	return s == StatusOnline || s == StatusOffline || s == StatusLinkedUp
}

// Signin makes an account the console's current one, and says whether that person chose Offline. An
// account new to this console and the consoles in with it appears on the owner's page, with no
// friends. Whoever was signed in here before shows Offline, unless they are on another console.
// joinsOnly is the app saying it never hosts, as 2P Start does.
func (h *Hub) Signin(number int, account, username, status string, joinsOnly bool) (string, bool, error) {
	if !cleanWord(account, maxAccount) || !cleanWord(username, maxUsername) {
		return "", false, ErrBadRequest
	}
	if status == "" {
		status = StatusOnline
	}
	if !cleanStatus(status) {
		return "", false, ErrBadRequest
	}
	id, chose := "", false
	err := h.store.Change(func(st *store.State) bool {
		p := st.PersonFor(number, account)
		if p == nil {
			if st.CountOn(number) >= peoplePerConsole {
				return false
			}
			st.People = append(st.People, store.Person{ID: store.NewID(), Console: number, Account: account, Username: username, LastSeen: h.now(), JoinsOnly: joinsOnly})
			id = st.People[len(st.People)-1].ID
			return true
		}
		id, chose = p.ID, p.ChoseOffline
		changed := p.Use(number)
		if p.Username != username {
			p.Username, changed = username, true
		}
		if p.JoinsOnly != joinsOnly {
			p.JoinsOnly, changed = joinsOnly, true
		}
		return changed
	})
	if err != nil {
		return "", false, err
	}
	if id == "" {
		return "", false, ErrBadRequest
	}
	h.touchSeen(id)
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	before := l.person
	l.person, l.status = id, status
	l.lastSeen, l.activeAt = h.now(), h.now()
	h.changedLocked(&st, id)
	if before != "" && before != id {
		h.changedLocked(&st, before)
		h.endInvitesFromLocked(before, number)
	}
	return id, chose, nil
}

// Signout leaves the console with nobody current.
func (h *Hub) Signout(number int) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	before := l.person
	l.person = ""
	if before != "" {
		h.changedLocked(&st, before)
		h.endInvitesFromLocked(before, number)
	}
}

// endInvitesFromLocked ends the invites someone sent from a console and nobody joined: they have
// left that console.
func (h *Hub) endInvitesFromLocked(person string, number int) {
	for _, inv := range h.invites {
		if inv.fromHere && inv.from == person && inv.fromConsole == number && !inv.joined {
			h.endInviteLocked(inv, EndCancelled, false)
		}
	}
}

// SetStatus is the account's own line on this console. A console also says Offline for its own
// reasons, in the same word. With chosen, Online or Offline is the person's own choice, which holds
// on every console they are on, and their other consoles hear it.
func (h *Hub) SetStatus(number int, status string, chosen bool) error {
	if !cleanStatus(status) {
		return ErrBadRequest
	}
	h.mu.Lock()
	l := h.liveFor(number, nil)
	person := l.person
	if person == "" {
		h.mu.Unlock()
		return ErrNoAccount
	}
	changed := l.status != status
	back := l.status == StatusOffline && status != StatusOffline
	l.status = status
	if back {
		l.activeAt = h.now()
	}
	h.mu.Unlock()

	// After the lock: these write to the disk. Coming back to a console makes it the one used last.
	choice := chosen && status != StatusLinkedUp
	chose := false
	if back || choice {
		h.store.Change(func(st *store.State) bool {
			p := st.Person(person)
			if p == nil {
				return false
			}
			moved := back && p.Use(number)
			if choice && p.ChoseOffline != (status == StatusOffline) {
				p.ChoseOffline, chose = status == StatusOffline, true
			}
			return moved || chose
		})
	}
	if !changed && !chose {
		return nil
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := st.Person(person); p != nil && chose {
		for _, n := range p.Consoles() {
			if o := h.lives[n]; n != number && o != nil && o.person == person {
				o.push(Event{Type: "chosen", Status: status})
			}
		}
	}
	h.changedLocked(&st, person)
	return nil
}

// Friends is the current account's list.
func (h *Hub) Friends(number int) ([]Friend, error) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	if l.person == "" {
		return nil, ErrNoAccount
	}
	out := []Friend{}
	size := 0
	withBox := func(i *FriendInvite) *FriendInvite {
		if i == nil {
			return nil
		}
		if size += len(i.Sealed); size > maxFriendsBytes {
			i.Sealed = ""
		}
		return i
	}
	for _, id := range st.FriendsOf(l.person) {
		p := st.Person(id)
		if p == nil {
			continue
		}
		at := h.keyConsoleLocked(p)
		f := Friend{ID: id, Username: p.Username, Server: st.Settings.Name, Status: h.statusLocked(&st, id)}
		if c := st.ConsoleByNumber(at); c != nil {
			f.Key = c.Key
		}
		if pl := h.lives[at]; pl != nil {
			f.Stream = pl.stream
		}
		f.Invite = withBox(h.inviteFromLocked(id, l.person, number))
		out = append(out, f)
	}
	for _, link := range st.Links {
		for _, pair := range link.Pairs {
			if pair.Ours != l.person {
				continue
			}
			f := Friend{ID: "p:" + pair.ID, Username: pair.TheirName, Server: link.Name, Status: StatusOffline, Key: pair.TheirKey, Stream: pair.TheirStream}
			if r := h.remotes[link.ID][pair.Theirs]; r != nil && !h.behind[link.ID] {
				f.Status = r.status
			}
			f.Invite = withBox(h.inviteFromLocked(pair.Theirs, l.person, number))
			out = append(out, f)
		}
	}
	return out, nil
}

// inviteFromLocked is a knock from one person to another that rings this console.
func (h *Hub) inviteFromLocked(from, to string, number int) *FriendInvite {
	for _, inv := range h.invites {
		if inv.from == from && inv.to == to && inv.toConsole == number && !inv.joined {
			return &FriendInvite{ID: inv.id, Sealed: inv.sealed}
		}
	}
	return nil
}

// Invite is a knock. It is refused on anyone Offline or Linked up, and it comes back as
// unavailable however that came about. The inviter picks the id, so the box can be bound to it, and
// a retry after a lost answer finds the same invite.
func (h *Hub) Invite(number int, id, friend, sealed string) (string, error) {
	if !goodInviteID(id) || sealed == "" || len(sealed) > maxInviteBox {
		return "", ErrBadRequest
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	l := h.liveFor(number, nil)
	me := l.person
	if me == "" {
		h.mu.Unlock()
		return "", ErrNoAccount
	}
	if old := h.invites[id]; old != nil {
		same := old.fromHere && old.from == me && old.sealed == sealed
		h.mu.Unlock()
		if same {
			return id, nil
		}
		return "", ErrUnavailable
	}
	if h.busyLocked(&st, me) {
		h.mu.Unlock()
		return "", ErrUnavailable
	}
	inv := &invite{id: id, from: me, fromConsole: number, fromHere: true, sealed: sealed, expires: h.now().Add(inviteLife)}

	if pairID, remoteFriend := cutPrefix(friend, "p:"); remoteFriend {
		link, pair := findPair(&st, me, pairID)
		if pair == nil {
			h.mu.Unlock()
			return "", ErrUnavailable
		}
		r := h.remotes[link.ID][pair.Theirs]
		if r == nil || r.status != StatusOnline || h.behind[link.ID] {
			h.mu.Unlock()
			return "", ErrUnavailable
		}
		// In the table before the call, so one account has one invite out however fast it asks, and a
		// join arriving straight away finds it.
		inv.to, inv.link = pair.Theirs, link.ID
		h.invites[id] = inv
		linker := h.links
		h.mu.Unlock()
		err := linker.Invite(link.ID, id, me, pair.Theirs, sealed)
		h.mu.Lock()
		defer h.mu.Unlock()
		if err != nil {
			delete(h.invites, id)
			return "", ErrUnavailable
		}
		h.changedLocked(&st, me)
		return id, nil
	}

	defer h.mu.Unlock()
	if !st.AreFriends(me, friend) || h.statusLocked(&st, friend) != StatusOnline || !h.knockAllowedLocked(me, friend) {
		return "", ErrUnavailable
	}
	inv.to, inv.toConsole = friend, h.keyConsoleLocked(st.Person(friend))
	h.invites[id] = inv
	h.pushOn(inv, friend, Event{Type: "invite", Invite: id, From: me, Sealed: sealed})
	h.changedLocked(&st, me)
	return id, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func findPair(st *store.State, me, pairID string) (*store.Link, *store.Pair) {
	for i := range st.Links {
		for j := range st.Links[i].Pairs {
			if p := &st.Links[i].Pairs[j]; p.ID == pairID && p.Ours == me {
				return &st.Links[i], p
			}
		}
	}
	return nil, nil
}

// busyLocked says whether someone already has an invite out, or a session.
func (h *Hub) busyLocked(st *store.State, id string) bool {
	for _, inv := range h.invites {
		if (inv.fromHere && inv.from == id) || (inv.joined && inv.to == id) {
			return true
		}
	}
	return false
}

// partOf finds an invite the console's current account is part of.
func (h *Hub) partOf(number int, id string) (*invite, string, error) {
	l := h.liveFor(number, nil)
	inv := h.invites[id]
	if l.person == "" {
		return nil, "", ErrNoAccount
	}
	if inv == nil {
		return nil, l.person, ErrUnavailable
	}
	if inv.consoleOf(l.person) != number {
		return nil, l.person, ErrNotYours
	}
	return inv, l.person, nil
}

// endInviteLocked ends an invite nobody joined and tells whoever needs telling. An invite that
// ends tells the other side once, by the one event.
func (h *Hub) endInviteLocked(inv *invite, reason string, tellInviter bool) {
	st := h.store.Snapshot()
	delete(h.invites, inv.id)
	if inv.fromHere && tellInviter {
		h.pushOn(inv, inv.from, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
	}
	if inv.link == "" {
		if reason != EndDeclined {
			h.pushOn(inv, inv.to, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
		}
	} else if inv.fromHere {
		h.links.InviteEnded(inv.link, inv.id, reason)
	} else if reason != EndDeclined {
		h.pushOn(inv, inv.to, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
	}
	h.relay.EndSession(inv.id)
	h.changedLocked(&st, inv.from)
}

// Cancel takes back an invite. If a join and a cancel cross, whichever reached the server first wins.
func (h *Hub) Cancel(number int, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, me, err := h.partOf(number, id)
	if err != nil {
		return err
	}
	if !inv.fromHere || inv.from != me {
		return ErrNotYours
	}
	if inv.joined {
		return ErrUnavailable
	}
	h.endInviteLocked(inv, EndCancelled, false)
	return nil
}

// Decline answers an invite with no.
func (h *Hub) Decline(number int, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, me, err := h.partOf(number, id)
	if err != nil {
		return err
	}
	if inv.to != me || inv.joined {
		return ErrNotYours
	}
	if !inv.fromHere {
		delete(h.invites, inv.id)
		h.links.InviteEnded(inv.link, inv.id, EndDeclined)
		return nil
	}
	h.endInviteLocked(inv, EndDeclined, true)
	return nil
}

// Join answers an invite with yes. Joining one ends any other.
func (h *Hub) Join(number int, id string) (string, error) {
	h.mu.Lock()
	inv, me, err := h.partOf(number, id)
	if err != nil {
		h.mu.Unlock()
		return "", err
	}
	if inv.to != me || inv.joined {
		h.mu.Unlock()
		return "", ErrUnavailable
	}
	if !inv.fromHere {
		// The inviter's server decides: it takes whichever of a join and a cancel came first.
		linker, link := h.links, inv.link
		h.mu.Unlock()
		if err := linker.Join(link, id); err != nil {
			h.mu.Lock()
			delete(h.invites, id)
			h.mu.Unlock()
			return "", ErrUnavailable
		}
		h.mu.Lock()
		inv = h.invites[id]
		if inv == nil {
			h.mu.Unlock()
			return "", ErrUnavailable
		}
	}
	defer h.mu.Unlock()
	st := h.store.Snapshot()
	inv.joined, inv.joinedAt = true, h.now()
	for _, other := range h.invites {
		if other == inv {
			continue
		}
		switch {
		case other.to == me && !other.joined && other.fromHere:
			h.endInviteLocked(other, EndUnavailable, true)
		case other.to == me && !other.joined:
			delete(h.invites, other.id)
			h.links.InviteEnded(other.link, other.id, EndUnavailable)
		case other.fromHere && other.from == me && !other.joined:
			h.endInviteLocked(other, EndCancelled, false)
		}
	}
	if inv.fromHere {
		h.pushOn(inv, inv.from, Event{Type: "joined", Session: id})
	}
	h.changedLocked(&st, me, inv.from)
	return id, nil
}

// Signal passes sealed connection details to the other console of a session.
func (h *Hub) Signal(number int, id, sealed string) error {
	if sealed == "" || len(sealed) > maxSignalBox {
		return ErrBadRequest
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, me, err := h.partOf(number, id)
	if err != nil {
		return err
	}
	if !inv.joined {
		return ErrUnavailable
	}
	other := inv.to
	if inv.to == me {
		other = inv.from
	}
	if inv.link != "" {
		h.links.Signal(inv.link, id, sealed)
		return nil
	}
	h.pushOn(inv, other, Event{Type: "signal", Session: id, Sealed: sealed})
	return nil
}

// End ends a session, from either console.
func (h *Hub) End(number int, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, me, err := h.partOf(number, id)
	if err != nil {
		return err
	}
	if !inv.joined {
		return ErrUnavailable
	}
	h.endSessionLocked(inv, me)
	return nil
}

func (h *Hub) endSessionLocked(inv *invite, by string) {
	st := h.store.Snapshot()
	delete(h.invites, inv.id)
	other := inv.to
	if inv.to == by {
		other = inv.from
	}
	if inv.link != "" {
		h.links.End(inv.link, inv.id)
		if st.Person(by) == nil {
			// The other server ended it: tell the one on this side.
			h.pushOn(inv, other, Event{Type: "session_ended", Session: inv.id})
		}
	} else {
		h.pushOn(inv, other, Event{Type: "session_ended", Session: inv.id})
	}
	h.relay.EndSession(inv.id)
	h.changedLocked(&st, inv.from, inv.to)
}

// Report records how a session connected, once. The words become fixed log lines.
func (h *Hub) Report(number int, id, path, reason string) error {
	if path != "direct" && path != "relay" && path != "failed" {
		return ErrBadRequest
	}
	h.mu.Lock()
	inv, _, err := h.partOf(number, id)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	if inv.reported {
		h.mu.Unlock()
		return nil
	}
	inv.reported = true
	relayed := inv.relayed
	h.mu.Unlock()
	// After the lock: these write to the disk.
	switch path {
	case "direct":
		h.log.GameDirect()
	case "relay":
		h.log.GameRelayed()
		if relayed {
			h.relay.AddGame()
		}
	case "failed":
		h.log.GameFailed(logfile.ReasonFromWord(reason))
	}
	h.store.Touch(func(s *store.State) bool {
		switch path {
		case "direct":
			s.Games.Direct++
		case "relay":
			s.Games.Relayed++
		case "failed":
			s.Games.Failed++
		}
		return true
	})
	return nil
}

// Relay gives a console a way through a relay for one session: the host's server's first, then
// the friend's. Nobody gets one for a game that does not involve this server's people.
func (h *Hub) Relay(number int, id string) (*relay.Credentials, string, error) {
	h.mu.Lock()
	inv, _, err := h.partOf(number, id)
	if err != nil {
		h.mu.Unlock()
		return nil, "", err
	}
	if !inv.joined {
		h.mu.Unlock()
		return nil, "", ErrUnavailable
	}
	link, fromHere := inv.link, inv.fromHere
	linker := h.links
	h.mu.Unlock()
	if link != "" && !fromHere {
		if creds, _ := linker.Relay(link, id); creds != nil {
			return creds, "", nil
		}
	}
	creds, why := h.localRelay(id)
	if creds != nil {
		return creds, "", nil
	}
	if link != "" && fromHere {
		if c, w := linker.Relay(link, id); c != nil {
			return c, "", nil
		} else if w != "" && why == "off" {
			why = w
		}
	}
	return nil, why, nil
}

func (h *Hub) localRelay(id string) (*relay.Credentials, string) {
	if why := h.relay.Available(); why != "" {
		return nil, why
	}
	c, err := h.relay.Credentials(id)
	if err != nil {
		return nil, err.Error()
	}
	h.mu.Lock()
	if inv := h.invites[id]; inv != nil {
		inv.relayed = true
	}
	h.mu.Unlock()
	return &c, ""
}

// Leave takes the console off the server, on its own word, and its accounts that are on no other
// console.
func (h *Hub) Leave(number int) error {
	touched := []string{}
	err := h.store.Update(func(st *store.State) error {
		for _, p := range st.People {
			if p.On(number) {
				touched = append(append(touched, p.ID), st.FriendsOf(p.ID)...)
			}
		}
		st.RemoveConsole(number)
		st.Consoles = slices.DeleteFunc(st.Consoles, func(c store.Console) bool { return c.Number == number })
		return nil
	})
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.endConsoleLocked(number)
	delete(h.lives, number)
	h.mu.Unlock()
	h.log.ConsoleRemoved(number)
	h.FriendsChanged(touched...)
	h.mu.Lock()
	linker := h.links
	h.mu.Unlock()
	go linker.Changed()
	return nil
}

// Wait is the one long request: what happened since `after`, or nothing after a while.
func (h *Hub) Wait(ctx context.Context, number, after int) (string, []Event, int) {
	h.mu.Lock()
	l := h.liveFor(number, nil)
	l.waiting++
	l.lastSeen = h.now()
	l.heard = after
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		l.waiting--
		l.lastSeen = h.now()
		h.mu.Unlock()
	}()

	deadline := time.NewTimer(waitHold)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		var out []Event
		size := 0
		next := l.next
		for _, e := range l.events {
			if e.Seq <= after {
				continue
			}
			size += len(e.Sealed) + 128
			if size > maxWaitBytes && len(out) > 0 {
				next = out[len(out)-1].Seq
				break
			}
			out = append(out, e)
		}
		h.mu.Unlock()
		if len(out) > 0 {
			return h.epoch, out, next
		}
		select {
		case <-l.wake:
		case <-deadline.C:
			return h.epoch, []Event{}, next
		case <-ctx.Done():
			return h.epoch, []Event{}, next
		}
	}
}

// ---- What a linked server says ----

// RemoteStatus saves what a linked server said about its people. Only people in a pair are
// listened to; anything else is dropped.
func (h *Hub) RemoteStatus(link string, behind bool, people map[string]string) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := st.Link(link)
	if l == nil {
		return
	}
	known := map[string]*remote{}
	touched := map[string]bool{}
	for _, pair := range l.Pairs {
		status, ok := people[pair.Theirs]
		if !ok || !cleanStatus(status) {
			status = StatusOffline
		}
		known[pair.Theirs] = &remote{status: status}
		old := h.remotes[link][pair.Theirs]
		if old == nil || old.status != status || h.behind[link] != behind {
			touched[pair.Ours] = true
		}
	}
	h.remotes[link] = known
	h.behind[link] = behind
	for id := range touched {
		h.pushTo(&st, id, Event{Type: "friends"})
	}
}

// RemoteInvite is an invite from someone on a linked server. It is refused unless those two
// people are a pair on that link, and the one here can be knocked on.
func (h *Hub) RemoteInvite(link, id, from, to, sealed string) error {
	if !goodInviteID(id) || sealed == "" || len(sealed) > maxInviteBox {
		return ErrBadRequest
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := st.Link(link)
	if l == nil || !slices.ContainsFunc(l.Pairs, func(p store.Pair) bool { return p.Ours == to && p.Theirs == from }) {
		return ErrUnavailable
	}
	if old := h.invites[id]; old != nil {
		if old.link == link && old.from == from && old.sealed == sealed {
			return nil
		}
		return ErrUnavailable
	}
	if h.statusLocked(&st, to) != StatusOnline || !h.knockAllowedLocked(link+"|"+from, to) {
		return ErrUnavailable
	}
	pairID := ""
	for _, p := range l.Pairs {
		if p.Ours == to && p.Theirs == from {
			pairID = p.ID
		}
	}
	inv := &invite{id: id, from: from, to: to, toConsole: h.keyConsoleLocked(st.Person(to)), link: link, sealed: sealed, expires: h.now().Add(inviteLife)}
	h.invites[id] = inv
	h.pushOn(inv, to, Event{Type: "invite", Invite: id, From: "p:" + pairID, Sealed: sealed})
	h.pushTo(&st, to, Event{Type: "friends"})
	return nil
}

// RemoteInviteEnded is an invite ending on the other server.
func (h *Hub) RemoteInviteEnded(link, id, reason string) {
	if reason != EndCancelled && reason != EndTimeout && reason != EndDeclined && reason != EndUnavailable {
		reason = EndUnavailable
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := h.invites[id]
	if inv == nil || inv.link != link || inv.joined {
		return
	}
	delete(h.invites, id)
	if inv.fromHere {
		h.pushOn(inv, inv.from, Event{Type: "invite_ended", Invite: id, Reason: reason})
		h.changedLocked(&st, inv.from)
	} else {
		h.pushOn(inv, inv.to, Event{Type: "invite_ended", Invite: id, Reason: reason})
		h.pushTo(&st, inv.to, Event{Type: "friends"})
	}
	h.relay.EndSession(id)
}

// RemoteJoin is the friend joining from the other server. This server is the inviter's, so it
// decides: whichever of a join and a cancel reached it first.
func (h *Hub) RemoteJoin(link, id string) error {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := h.invites[id]
	if inv == nil || inv.link != link || !inv.fromHere {
		return ErrUnavailable
	}
	if inv.joined {
		// A retry after a lost answer: that join already won.
		return nil
	}
	inv.joined, inv.joinedAt = true, h.now()
	h.pushOn(inv, inv.from, Event{Type: "joined", Session: id})
	h.changedLocked(&st, inv.from)
	return nil
}

// RemoteSignal is sealed connection details from the other server's console.
func (h *Hub) RemoteSignal(link, id, sealed string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := h.invites[id]
	if inv == nil || inv.link != link || !inv.joined || len(sealed) > maxSignalBox {
		return
	}
	here := inv.from
	if !inv.fromHere {
		here = inv.to
	}
	h.pushOn(inv, here, Event{Type: "signal", Session: id, Sealed: sealed})
}

// RemoteEnd is a session ended on the other server.
func (h *Hub) RemoteEnd(link, id string) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := h.invites[id]
	if inv == nil || inv.link != link {
		return
	}
	delete(h.invites, id)
	here := inv.from
	if !inv.fromHere {
		here = inv.to
	}
	h.pushOn(inv, here, Event{Type: "session_ended", Session: id})
	h.relay.EndSession(id)
	h.changedLocked(&st, here)
}

// RemoteRelay gives the other server's console a way through this server's relay, for a session
// that involves one of this server's people, and nothing else.
func (h *Hub) RemoteRelay(link, id string) (*relay.Credentials, string) {
	h.mu.Lock()
	inv := h.invites[id]
	ok := inv != nil && inv.link == link && inv.joined
	h.mu.Unlock()
	if !ok {
		return nil, "off"
	}
	return h.localRelay(id)
}

// noLinks stands in until the links are set.
type noLinks struct{}

func (noLinks) Changed()                                            {}
func (noLinks) Invite(string, string, string, string, string) error { return ErrUnavailable }
func (noLinks) InviteEnded(string, string, string)                  {}
func (noLinks) Join(string, string) error                           { return ErrUnavailable }
func (noLinks) Signal(string, string, string)                       {}
func (noLinks) End(string, string)                                  {}
func (noLinks) Relay(string, string) (*relay.Credentials, string)   { return nil, "off" }
