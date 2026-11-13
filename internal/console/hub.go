// Package console is what consoles talk to: pairing, presence, invites and connection details,
// one long request at a time. Each file has one job:
//
//   - handler.go: the port, the sessions after pairing, and each request a console makes
//   - pairing.go: a console or a server using a code, and the limits on guessing one
//   - hub.go: the Hub, which the files below are part of, and what runs out of time
//   - presence.go: who is on, each person's status, which console a knock rings, and joining
//   - events.go: what a console hears, on its one long request
//   - friends.go: a console's list of friends
//   - invites.go: a knock and its answer
//   - sessions.go: a game's signals, relay and report, and ending what was removed or left
//   - remote.go: what a linked server says
//
// handler.go moves sealed bytes; the hub is the people. The server owns everything about people: each account's status, who can be invited, refusing
// a knock on anyone Offline or Linked up, the invite's 30 minutes, declines, ending a second
// invite. The console owns the TV and the game. So the hub says what happened and never what to
// display. All of it stays in memory: a restart clears who is online and every invite.
package console

import (
	"context"
	"errors"
	"sync"
	"time"

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

// recent keeps the times after cutoff, in the list's own memory. Pairing's limits on guessing a code
// and the limit on knocks both count with it.
func recent(list []time.Time, cutoff time.Time) []time.Time {
	kept := list[:0]
	for _, at := range list {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
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
