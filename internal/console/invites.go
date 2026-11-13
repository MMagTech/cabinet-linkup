package console

import (
	"encoding/hex"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/store"
)

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

// endInvitesFromLocked ends the invites someone sent from a console and nobody joined: they have
// left that console.
func (h *Hub) endInvitesFromLocked(person string, number int) {
	for _, inv := range h.invites {
		if inv.fromHere && inv.from == person && inv.fromConsole == number && !inv.joined {
			h.endInviteLocked(inv, EndCancelled, false)
		}
	}
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
