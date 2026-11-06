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
	wake     chan struct{}
}

// invite is one invite, and then the session it became. Either end can be on a linked server,
// and then its person is an id on that server and link says which server. Its id is the same on
// both servers, made by the inviter's.
type invite struct {
	id       string
	from, to string
	link     string
	fromHere bool // the inviter is on this server, and this server decides
	sealed   string
	expires  time.Time
	joined   bool
	relayed  bool // this server's relay gave out a password for it
	reported bool
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
		if l.person != "" && !h.liveLocked(l, now) && l.status != StatusOffline {
			touched[l.person] = true
			l.status = StatusOffline
			for _, inv := range h.invites {
				if inv.fromHere && inv.from == l.person && !inv.joined {
					h.endInviteLocked(inv, EndCancelled, false)
				}
			}
		}
	}
	for _, inv := range h.invites {
		if !inv.joined && now.After(inv.expires) && inv.fromHere {
			h.endInviteLocked(inv, EndTimeout, true)
			touched[inv.from] = true
		}
	}
	for id := range touched {
		h.changedLocked(&st, id)
	}
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
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	l.lastSeen = h.now()
	if l.person != "" {
		h.touchSeen(l.person)
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

// statusLocked is what a person on this server is right now.
func (h *Hub) statusLocked(st *store.State, id string) string {
	p := st.Person(id)
	if p == nil {
		return StatusOffline
	}
	l := h.lives[p.Console]
	if l == nil || l.person != id || !h.liveLocked(l, h.now()) || l.status == StatusOffline {
		return StatusOffline
	}
	if l.status == StatusLinkedUp {
		return StatusLinkedUp
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

// StreamOf is the stream version a person's console last stated.
func (h *Hub) StreamOf(number int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.lives[number]; l != nil {
		return l.stream
	}
	return 0
}

// push adds an event to a console's queue and wakes its long request.
func (l *live) push(e Event) {
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

// pushTo adds an event for a person, if their console is listening and they are its current one.
func (h *Hub) pushTo(st *store.State, person string, e Event) {
	p := st.Person(person)
	if p == nil {
		return
	}
	if l := h.lives[p.Console]; l != nil && l.person == person {
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

// ConsoleRemoved tells a console it was taken off the server, and ends what it was part of.
func (h *Hub) ConsoleRemoved(number int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lives[number]
	if l == nil {
		return
	}
	person := l.person
	l.person = ""
	l.status = StatusOffline
	l.push(Event{Type: "removed"})
	if person != "" {
		h.endPersonLocked(person)
	}
}

// endPersonLocked ends every invite and session a person is part of.
func (h *Hub) endPersonLocked(person string) {
	for _, inv := range h.invites {
		mine := (inv.fromHere && inv.from == person) || (!inv.fromHere && inv.to == person) || (inv.link == "" && inv.to == person)
		if !mine {
			continue
		}
		if inv.joined {
			h.endSessionLocked(inv, person)
		} else {
			h.endInviteLocked(inv, EndCancelled, false)
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

// Signin makes an account the console's current one. A new account appears on the owner's page,
// with no friends. Whoever was signed in before shows Offline.
func (h *Hub) Signin(number int, account, username, status string) (string, error) {
	if !cleanWord(account, maxAccount) || !cleanWord(username, maxUsername) {
		return "", ErrBadRequest
	}
	if status == "" {
		status = StatusOnline
	}
	if !cleanStatus(status) {
		return "", ErrBadRequest
	}
	id := ""
	err := h.store.Update(func(st *store.State) error {
		p := st.PersonByAccount(number, account)
		if p == nil {
			count := 0
			for _, x := range st.People {
				if x.Console == number {
					count++
				}
			}
			if count >= peoplePerConsole {
				return ErrBadRequest
			}
			st.People = append(st.People, store.Person{ID: store.NewID(), Console: number, Account: account, Username: username, LastSeen: h.now()})
			id = st.People[len(st.People)-1].ID
			return nil
		}
		id = p.ID
		p.Username = username
		p.LastSeen = h.now()
		return nil
	})
	if err != nil {
		return "", err
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	before := l.person
	l.person, l.status = id, status
	l.lastSeen = h.now()
	h.changedLocked(&st, id)
	if before != "" && before != id {
		h.changedLocked(&st, before)
		h.endInvitesFromLocked(before)
	}
	return id, nil
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
		h.endInvitesFromLocked(before)
	}
}

// endInvitesFromLocked ends the invites someone sent and nobody joined: they have left the console.
func (h *Hub) endInvitesFromLocked(person string) {
	for _, inv := range h.invites {
		if inv.fromHere && inv.from == person && !inv.joined {
			h.endInviteLocked(inv, EndCancelled, false)
		}
	}
}

// SetStatus is the account's own line. A console also says Offline for its own reasons, in one word.
func (h *Hub) SetStatus(number int, status string) error {
	if !cleanStatus(status) {
		return ErrBadRequest
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	if l.person == "" {
		return ErrNoAccount
	}
	if l.status != status {
		l.status = status
		h.changedLocked(&st, l.person)
	}
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
	for _, id := range st.FriendsOf(l.person) {
		p := st.Person(id)
		if p == nil {
			continue
		}
		c := st.ConsoleByNumber(p.Console)
		f := Friend{ID: id, Username: p.Username, Server: st.Settings.Name, Status: h.statusLocked(&st, id)}
		if c != nil {
			f.Key = c.Key
		}
		if pl := h.lives[p.Console]; pl != nil {
			f.Stream = pl.stream
		}
		f.Invite = h.inviteFromLocked(id, l.person)
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
			f.Invite = h.inviteFromLocked(pair.Theirs, l.person)
			out = append(out, f)
		}
	}
	return out, nil
}

func (h *Hub) inviteFromLocked(from, to string) *FriendInvite {
	for _, inv := range h.invites {
		if inv.from == from && inv.to == to && !inv.joined {
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
	inv := &invite{id: id, from: me, fromHere: true, sealed: sealed, expires: h.now().Add(inviteLife)}

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
		linker := h.links
		h.mu.Unlock()
		if err := linker.Invite(link.ID, id, me, pair.Theirs, sealed); err != nil {
			return "", ErrUnavailable
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		inv.to, inv.link = pair.Theirs, link.ID
		h.invites[id] = inv
		h.changedLocked(&st, me)
		return id, nil
	}

	defer h.mu.Unlock()
	if !st.AreFriends(me, friend) || h.statusLocked(&st, friend) != StatusOnline || !h.knockAllowedLocked(me, friend) {
		return "", ErrUnavailable
	}
	inv.to = friend
	h.invites[id] = inv
	h.pushTo(&st, friend, Event{Type: "invite", Invite: id, From: me, Sealed: sealed})
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
	here := (inv.fromHere && inv.from == l.person) || (!inv.fromHere && inv.to == l.person) || (inv.fromHere && inv.link == "" && inv.to == l.person)
	if !here {
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
		h.pushTo(&st, inv.from, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
	}
	if inv.link == "" {
		if reason != EndDeclined {
			h.pushTo(&st, inv.to, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
		}
	} else if inv.fromHere {
		h.links.InviteEnded(inv.link, inv.id, reason)
	} else if reason != EndDeclined {
		h.pushTo(&st, inv.to, Event{Type: "invite_ended", Invite: inv.id, Reason: reason})
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
	inv.joined = true
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
		h.pushTo(&st, inv.from, Event{Type: "joined", Session: id})
	}
	h.changedLocked(&st, me, inv.from)
	return id, nil
}

// Signal passes sealed connection details to the other console of a session.
func (h *Hub) Signal(number int, id, sealed string) error {
	if sealed == "" || len(sealed) > maxSignalBox {
		return ErrBadRequest
	}
	st := h.store.Snapshot()
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
	h.pushTo(&st, other, Event{Type: "signal", Session: id, Sealed: sealed})
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
			h.pushTo(&st, other, Event{Type: "session_ended", Session: inv.id})
		}
	} else {
		h.pushTo(&st, other, Event{Type: "session_ended", Session: inv.id})
	}
	h.relay.EndSession(inv.id)
	h.changedLocked(&st, inv.from, inv.to)
}

// Report records how a session connected, once. The words become fixed log lines.
func (h *Hub) Report(number int, id, path, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, _, err := h.partOf(number, id)
	if err != nil {
		return err
	}
	if inv.reported {
		return nil
	}
	switch path {
	case "direct":
		h.log.GameDirect()
	case "relay":
		h.log.GameRelayed()
		if inv.relayed {
			h.relay.AddGame()
		}
	case "failed":
		h.log.GameFailed(logfile.ReasonFromWord(reason))
	default:
		return ErrBadRequest
	}
	inv.reported = true
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

// Leave takes the console and its accounts off the server, on its own word.
func (h *Hub) Leave(number int) error {
	var gone []string
	err := h.store.Update(func(st *store.State) error {
		for _, p := range st.People {
			if p.Console == number {
				gone = append(gone, p.ID)
			}
		}
		st.Consoles = slices.DeleteFunc(st.Consoles, func(c store.Console) bool { return c.Number == number })
		st.Codes = slices.DeleteFunc(st.Codes, func(c store.Code) bool { return c.Kind == store.CodeConsole && c.Console == number })
		return nil
	})
	if err != nil {
		return err
	}
	st := h.store.Snapshot()
	h.mu.Lock()
	touched := []string{}
	for _, id := range gone {
		touched = append(touched, st.FriendsOf(id)...)
		h.endPersonLocked(id)
	}
	delete(h.lives, number)
	h.mu.Unlock()
	h.store.Update(func(s *store.State) error {
		for _, id := range gone {
			s.RemovePerson(id)
		}
		return nil
	})
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
	h.invites[id] = &invite{id: id, from: from, to: to, link: link, sealed: sealed, expires: h.now().Add(inviteLife)}
	h.pushTo(&st, to, Event{Type: "invite", Invite: id, From: "p:" + pairID, Sealed: sealed})
	h.pushTo(&st, to, Event{Type: "friends"})
	return nil
}

// RemoteInviteEnded is an invite ending on the other server.
func (h *Hub) RemoteInviteEnded(link, id, reason string) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := h.invites[id]
	if inv == nil || inv.link != link || inv.joined {
		return
	}
	delete(h.invites, id)
	if inv.fromHere {
		h.pushTo(&st, inv.from, Event{Type: "invite_ended", Invite: id, Reason: reason})
		h.changedLocked(&st, inv.from)
	} else {
		h.pushTo(&st, inv.to, Event{Type: "invite_ended", Invite: id, Reason: reason})
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
	if inv == nil || inv.link != link || !inv.fromHere || inv.joined {
		return ErrUnavailable
	}
	inv.joined = true
	h.pushTo(&st, inv.from, Event{Type: "joined", Session: id})
	h.changedLocked(&st, inv.from)
	return nil
}

// RemoteSignal is sealed connection details from the other server's console.
func (h *Hub) RemoteSignal(link, id, sealed string) {
	st := h.store.Snapshot()
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
	h.pushTo(&st, here, Event{Type: "signal", Session: id, Sealed: sealed})
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
	h.pushTo(&st, here, Event{Type: "session_ended", Session: id})
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
