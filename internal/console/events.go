package console

import (
	"context"
	"slices"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/store"
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
