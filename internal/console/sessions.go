package console

import (
	"slices"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

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
