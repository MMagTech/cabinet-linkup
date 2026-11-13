package console

import (
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/store"
	"github.com/MMagTech/cabinet-linkup/internal/text"
)

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

// cleanWord holds a word a console sends to its limit in bytes, on one line.
func cleanWord(s string, limit int) bool {
	return s != "" && len(s) <= limit && text.Plain(s)
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

// joinConsole is a console using a code. A new console gets a number, and a console that was signed
// out gets its own back. The account signed in at this moment is the friend, or, when
// it is the very account the invite was made for, that person on one more console. An invite or a
// console's own code then becomes that person's own code, unless they have one. A person's own code
// works only for their account, and is never used up.
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
		owner := st.Person(c.For)
		if c.Kind == store.CodePerson && (owner == nil || owner.Account != j.Account) {
			return ErrUnavailable
		}
		known := st.ConsoleByKey(key)
		switch c.Kind {
		case store.CodeOwn, store.CodeInvite, store.CodePerson:
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
		p := st.PersonFor(number, j.Account)
		switch {
		case c.Kind == store.CodePerson:
			p = owner
		case p == nil && c.Kind == store.CodeInvite && owner != nil && owner.Account == j.Account:
			p = owner
		}
		if p == nil {
			st.People = append(st.People, store.Person{ID: store.NewID(), Console: number, Account: j.Account, Username: j.Username, LastSeen: h.now()})
			p = &st.People[len(st.People)-1]
		} else {
			p.Use(number)
			p.Username, p.LastSeen = j.Username, h.now()
		}
		p.JoinsOnly = joinsOnly(j.Hosts)
		person = p.ID
		if f := st.Person(c.For); c.Kind == store.CodeInvite && f != nil && c.For != p.ID {
			if store.CanPlay(p, f) {
				friend = c.For
				st.AddFriends(p.ID, friend)
			}
			if c.Note != "" && p.Note == "" {
				p.Note = c.Note
			}
		}
		switch {
		case c.Kind == store.CodePerson:
		case (c.Kind == store.CodeInvite || c.Kind == store.CodeOwn) && st.CodeOf(p.ID) == nil:
			// The code they used becomes their own, for each of their consoles, until it is changed.
			k := st.Code(c.ID)
			k.Kind, k.For, k.Note, k.Expires = store.CodePerson, p.ID, "", time.Time{}
		default:
			st.RemoveCode(c.ID)
		}
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
