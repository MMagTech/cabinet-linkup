package console

import (
	"slices"

	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

// remote is what a linked server last said about one of its people.
type remote struct {
	status string
}

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
