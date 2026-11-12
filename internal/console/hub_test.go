package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

type fakeRelay struct {
	why   string
	ended []string
	games int
}

func (f *fakeRelay) Available() string { return f.why }
func (f *fakeRelay) Credentials(session string) (relay.Credentials, error) {
	return relay.Credentials{Username: "u." + session, Password: "p"}, nil
}
func (f *fakeRelay) EndSession(s string) { f.ended = append(f.ended, s) }
func (f *fakeRelay) AddGame()            { f.games++ }

type hubRig struct {
	t     *testing.T
	h     *Hub
	st    *store.Store
	relay *fakeRelay
	log   *logfile.Log
	clock time.Time
	mu    sync.Mutex
}

func newHubRig(t *testing.T) *hubRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, func(*store.State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	log, err := logfile.Open(filepath.Join(dir, "log.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &hubRig{t: t, st: st, relay: &fakeRelay{}, log: log, clock: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	r.h = NewHub(st, log, r.relay)
	r.h.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }
	return r
}

func (r *hubRig) advance(d time.Duration) {
	r.mu.Lock()
	r.clock = r.clock.Add(d)
	r.mu.Unlock()
}

// console adds a paired console and signs one account in, Online.
func (r *hubRig) console(number int, username string) string {
	r.t.Helper()
	r.st.Update(func(st *store.State) error {
		if st.ConsoleByNumber(number) == nil {
			st.Consoles = append(st.Consoles, store.Console{Number: number, Key: []byte{byte(number)}})
		}
		return nil
	})
	r.h.Connected(number, []byte{byte(number)}, 1)
	id, _, err := r.h.Signin(number, username, username, StatusOnline)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *hubRig) friends(a, b string) {
	r.st.Update(func(st *store.State) error { st.AddFriends(a, b); return nil })
}

func (r *hubRig) events(number int) []Event {
	r.t.Helper()
	l := r.h.lives[number]
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	out := append([]Event(nil), l.events...)
	l.events = nil
	return out
}

func newInviteID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hasEvent(events []Event, kind, reason string) bool {
	for _, e := range events {
		if e.Type == kind && e.Reason == reason {
			return true
		}
	}
	return false
}

func statusOf(t *testing.T, h *Hub, number int, friend string) string {
	t.Helper()
	list, err := h.Friends(number)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.ID == friend {
			return f.Status
		}
	}
	t.Fatalf("%s is not on the list", friend)
	return ""
}

func TestAKidSeesOnlyTheirOwnFriends(t *testing.T) {
	r := newHubRig(t)
	parent := r.console(1, "parent")
	alex := r.console(2, "alex")
	theo := r.console(3, "theo")
	r.friends(parent, alex)
	kid, _, err := r.h.Signin(1, "kid", "kid", StatusOnline)
	if err != nil {
		t.Fatal(err)
	}
	r.friends(kid, theo)
	list, _ := r.h.Friends(1)
	if len(list) != 1 || list[0].ID != theo {
		t.Errorf("the kid sees %+v", list)
	}
	r.h.Signin(1, "parent", "parent", StatusOnline)
	list, _ = r.h.Friends(1)
	if len(list) != 1 || list[0].ID != alex {
		t.Errorf("the parent sees %+v", list)
	}
}

func TestTwoFriendsOfOnePersonDoNotSeeEachOther(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, b)
	r.friends(a, c)
	list, _ := r.h.Friends(2)
	if len(list) != 1 || list[0].ID != a {
		t.Errorf("%+v", list)
	}
}

func TestANewAccountAppearsWithNoFriends(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "parent")
	id, _, err := r.h.Signin(1, "second", "second", StatusOnline)
	if err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if p := st.Person(id); p == nil || p.Username != "second" || p.LastSeen.IsZero() {
		t.Fatalf("%+v", p)
	}
	if list, _ := r.h.Friends(1); len(list) != 0 {
		t.Errorf("%+v", list)
	}
}

func TestSameAccountKeepsItsPersonAndLastUsername(t *testing.T) {
	r := newHubRig(t)
	id := r.console(1, "alex")
	again, _, err := r.h.Signin(1, "alex", "alexander", StatusOnline)
	if err != nil || again != id {
		t.Fatalf("%v %v", again, err)
	}
	st := r.st.Snapshot()
	if st.Person(id).Username != "alexander" {
		t.Error("the server keeps the last username it was told")
	}
}

func TestSigninRefusesNonsense(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "a")
	for _, c := range [][3]string{{"", "x", ""}, {"x", "", ""}, {"x", "bad\nname", ""}, {"x", "y", "weird"}} {
		if _, _, err := r.h.Signin(1, c[0], c[1], c[2]); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%q: %v", c, err)
		}
	}
	for i := range peoplePerConsole + 2 {
		r.h.Signin(1, string(rune('A'+i)), "n", StatusOnline)
	}
	st := r.st.Snapshot()
	if len(st.People) > peoplePerConsole {
		t.Errorf("%d people on one console", len(st.People))
	}
}

func TestStatusShowsOfflineForEveryReason(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	if got := statusOf(t, r.h, 1, b); got != StatusOnline {
		t.Fatal(got)
	}

	r.h.SetStatus(2, StatusOffline, false)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("appearing offline")
	}
	r.h.SetStatus(2, StatusOnline, false)
	if got := statusOf(t, r.h, 1, b); got != StatusOnline {
		t.Error("one press brings them back")
	}

	other, _, _ := r.h.Signin(2, "other", "other", StatusOnline)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("someone else signed in on their console")
	}
	if !r.h.Online(other) || r.h.Online(b) {
		t.Error("the page must agree")
	}
	r.h.Signin(2, "b", "b", StatusOnline)

	r.h.Signout(2)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("signed out")
	}
	r.h.Signin(2, "b", "b", StatusOnline)

	r.advance(consoleSilence + time.Second)
	r.h.Seen(1)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("a console that stopped asking")
	}
}

func TestStatusIsOneAccountsOwn(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "parent")
	alex := r.console(2, "alex")
	r.friends(r.st.Snapshot().People[0].ID, alex)
	r.h.SetStatus(2, StatusOffline, false)
	kid, _, _ := r.h.Signin(2, "kid", "kid", StatusOnline)
	if !r.h.Online(kid) {
		t.Error("one account appearing offline holds for it and not another")
	}
}

func TestNoEventForComingOnline(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	r.events(1)
	r.h.SetStatus(2, StatusOffline, false)
	r.h.SetStatus(2, StatusOnline, false)
	for _, e := range r.events(1) {
		if e.Type != "friends" || e.Invite != "" || e.From != "" {
			t.Errorf("an event must say nothing about who: %+v", e)
		}
	}
}

func TestInviteOnlyToAnOnlineFriend(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	stranger := r.console(3, "stranger")
	r.friends(a, b)
	if _, err := r.h.Invite(1, newInviteID(), stranger, "sealed"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("not a friend: %v", err)
	}
	r.h.SetStatus(2, StatusOffline, false)
	if _, err := r.h.Invite(1, newInviteID(), b, "sealed"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("offline: %v", err)
	}
	r.h.SetStatus(2, StatusOnline, false)
	if _, err := r.h.Invite(1, newInviteID(), b, ""); !errors.Is(err, ErrBadRequest) {
		t.Errorf("nothing sealed: %v", err)
	}
	id, err := r.h.Invite(1, newInviteID(), b, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	events := r.events(2)
	found := false
	for _, e := range events {
		if e.Type == "invite" && e.Invite == id && e.From == a && e.Sealed == "sealed" {
			found = true
		}
	}
	if !found {
		t.Errorf("%+v", events)
	}
	list, _ := r.h.Friends(2)
	if list[0].Invite == nil || list[0].Invite.ID != id || list[0].Status != StatusLinkedUp {
		t.Errorf("%+v", list[0])
	}
}

func TestLinkedUpRefusesAKnock(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, b)
	r.friends(c, b)
	r.friends(c, a)
	if _, err := r.h.Invite(1, newInviteID(), b, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.Invite(3, newInviteID(), a, "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("someone inviting is Linked up: %v", err)
	}
	if _, err := r.h.Invite(1, newInviteID(), c, "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("one invite out at a time: %v", err)
	}
}

func TestTwoInvitesCrossing(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	if _, err := r.h.Invite(1, newInviteID(), b, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.Invite(2, newInviteID(), a, "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the second knock is refused: %v", err)
	}
}

func TestJoiningOneEndsAnyOther(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, c)
	r.friends(b, c)
	first, _ := r.h.Invite(1, newInviteID(), c, "s")
	second, err := r.h.Invite(2, newInviteID(), c, "s")
	if err != nil {
		t.Fatal(err)
	}
	r.events(1)
	r.events(2)
	r.events(3)
	if _, err := r.h.Join(3, first); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(2), "invite_ended", EndUnavailable) {
		t.Error("the other inviter hears")
	}
	if _, err := r.h.Join(3, second); err == nil {
		t.Error("the other invite is gone")
	}
	got := false
	for _, e := range r.events(1) {
		if e.Type == "joined" && e.Session == first {
			got = true
		}
	}
	if !got {
		t.Error("the inviter hears of the join")
	}
	if statusOf(t, r.h, 1, c) != StatusLinkedUp {
		t.Error("both show Linked up")
	}
}

func TestOnlyTheInvitedCanJoin(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, b)
	r.friends(a, c)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	if _, err := r.h.Join(3, id); err == nil {
		t.Error("someone else joined")
	}
	if _, err := r.h.Join(1, id); err == nil {
		t.Error("the inviter joined their own")
	}
	if err := r.h.Decline(3, id); err == nil {
		t.Error("someone else declined")
	}
	if err := r.h.Cancel(2, id); err == nil {
		t.Error("the friend cancelled")
	}
	if err := r.h.Signal(3, id, "s"); err == nil {
		t.Error("someone else signalled")
	}
	if _, _, err := r.h.Relay(3, id); err == nil {
		t.Error("someone else got the relay")
	}
}

func TestCancelDeclineAndThirtyMinutes(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)

	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.events(2)
	if err := r.h.Cancel(1, id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(2), "invite_ended", EndCancelled) {
		t.Error("cancel")
	}
	if statusOf(t, r.h, 2, a) != StatusOnline {
		t.Error("nobody joined, so they are Online again")
	}

	id, _ = r.h.Invite(1, newInviteID(), b, "s")
	r.events(1)
	if err := r.h.Decline(2, id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(1), "invite_ended", EndDeclined) {
		t.Error("decline")
	}

	id, _ = r.h.Invite(1, newInviteID(), b, "s")
	r.events(1)
	r.events(2)
	r.advance(inviteLife - time.Minute)
	r.h.Seen(1)
	r.h.Seen(2)
	r.h.Tick()
	if len(r.events(1)) != 0 {
		t.Error("too soon")
	}
	r.advance(2 * time.Minute)
	r.h.Seen(1)
	r.h.Seen(2)
	r.h.Tick()
	if !hasEvent(r.events(1), "invite_ended", EndTimeout) {
		t.Error("thirty minutes, inviter")
	}
	if !hasEvent(r.events(2), "invite_ended", EndTimeout) {
		t.Error("thirty minutes, friend")
	}
	if _, err := r.h.Join(2, id); err == nil {
		t.Error("an ended invite cannot be joined")
	}
}

func TestJoinAndCancelCrossing(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	if _, err := r.h.Join(2, id); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Cancel(1, id); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the join came first: %v", err)
	}
	id2 := ""
	r.h.End(1, id)
	id2, _ = r.h.Invite(1, newInviteID(), b, "s")
	r.h.Cancel(1, id2)
	if _, err := r.h.Join(2, id2); err == nil {
		t.Error("the cancel came first")
	}
}

func TestSessionSignalsAndEnd(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	if err := r.h.Signal(1, id, "early"); err == nil {
		t.Error("no connection details before a join")
	}
	r.h.Join(2, id)
	r.events(1)
	r.events(2)
	if err := r.h.Signal(1, id, "offer"); err != nil {
		t.Fatal(err)
	}
	if e := r.events(2); len(e) != 1 || e[0].Sealed != "offer" || e[0].Type != "signal" {
		t.Errorf("%+v", e)
	}
	if err := r.h.Signal(2, id, "answer"); err != nil {
		t.Fatal(err)
	}
	if e := r.events(1); len(e) != 1 || e[0].Sealed != "answer" {
		t.Errorf("%+v", e)
	}
	if err := r.h.End(2, id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(1), "session_ended", "") {
		t.Error("the other console hears")
	}
	if len(r.relay.ended) != 1 || r.relay.ended[0] != id {
		t.Errorf("the relay's password stops: %v", r.relay.ended)
	}
	if statusOf(t, r.h, 1, b) != StatusOnline {
		t.Error("online again")
	}
}

func TestReportCountsOnce(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	if err := r.h.Report(1, id, "nonsense", ""); !errors.Is(err, ErrBadRequest) {
		t.Error(err)
	}
	r.h.Report(1, id, "direct", "none")
	r.h.Report(2, id, "direct", "none")
	st := r.st.Snapshot()
	if st.Games.Direct != 1 {
		t.Errorf("both consoles report one game: %+v", st.Games)
	}
}

func TestReportRelayedGameCountsForTheMonth(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	creds, why, err := r.h.Relay(1, id)
	if err != nil || creds == nil || why != "" {
		t.Fatalf("%v %q %v", creds, why, err)
	}
	r.h.Report(1, id, "relay", "none")
	if r.relay.games != 1 {
		t.Errorf("%d", r.relay.games)
	}
	st := r.st.Snapshot()
	if st.Games.Relayed != 1 {
		t.Errorf("%+v", st.Games)
	}
}

func TestReportFailedReasonIsAFixedWord(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	r.h.Report(1, id, "failed", "alex at 203.0.113.5")
	for _, l := range r.log.Tail(10) {
		if l.Line != "game failed: other" {
			t.Errorf("%q", l.Line)
		}
	}
}

func TestRelayNotOfferedWhenOffOrFull(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	if _, _, err := r.h.Relay(1, id); !errors.Is(err, ErrUnavailable) {
		t.Errorf("not before a join: %v", err)
	}
	r.h.Join(2, id)
	for _, why := range []string{"off", "limit"} {
		r.relay.why = why
		creds, got, err := r.h.Relay(1, id)
		if err != nil || creds != nil || got != why {
			t.Errorf("%v %q %v", creds, got, err)
		}
	}
}

func TestRemovingAConsole(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.events(1)
	r.events(2)
	r.h.ConsoleRemoved(1)
	if !hasEvent(r.events(1), "removed", "") {
		t.Error("the console hears")
	}
	if !hasEvent(r.events(2), "invite_ended", EndCancelled) {
		t.Error("its invite ends")
	}
	if _, err := r.h.Join(2, id); err == nil {
		t.Error("gone")
	}
}

// The game does not go through the server, so it carries on with no word to anyone. Its relay stops,
// and every account on that console is finished, not only the one signed in.
func TestRemovingAConsoleMidGame(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	other, _, _ := r.h.Signin(2, "other", "other", StatusOnline)
	r.friends(a, other)
	r.h.Signin(2, "b", "b", StatusOnline)
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	r.events(1)
	r.h.ConsoleRemoved(2)
	if hasEvent(r.events(1), "session_ended", "") {
		t.Error("the game carries on: nothing says it ended")
	}
	if len(r.relay.ended) == 0 {
		t.Error("its relay stops")
	}
	if len(r.h.invites) != 0 {
		t.Error("nothing is left open")
	}
}

func TestLeaving(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	r.events(1)
	if err := r.h.Leave(2); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if st.Person(b) != nil || st.ConsoleByNumber(2) != nil || st.AreFriends(a, b) {
		t.Errorf("%+v", st)
	}
	if list, _ := r.h.Friends(1); len(list) != 0 {
		t.Errorf("%+v", list)
	}
	if !hasEvent(r.events(1), "friends", "") {
		t.Error("their friend's list changed")
	}
}

func TestWaitWakesAndHolds(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	r.events(1)
	got := make(chan []Event, 1)
	go func() {
		_, events, _ := r.h.Wait(context.Background(), 1, 1000)
		got <- events
	}()
	time.Sleep(50 * time.Millisecond)
	r.h.mu.Lock()
	r.h.lives[1].next = 1000
	r.h.lives[1].push(Event{Type: "friends"})
	r.h.mu.Unlock()
	select {
	case events := <-got:
		if len(events) != 1 || events[0].Seq != 1001 {
			t.Errorf("%+v", events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("it never woke")
	}
}

func TestWaitCountsAsOn(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.h.Wait(ctx, 2, 1<<30); close(done) }()
	time.Sleep(50 * time.Millisecond)
	r.advance(consoleSilence * 2)
	r.h.Seen(1)
	if got := statusOf(t, r.h, 1, b); got != StatusOnline {
		t.Error("a console waiting is on")
	}
	cancel()
	<-done
}

func TestEpochAndCursorAfterARestart(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first, _, _ := r.h.Wait(ctx, 1, 0)
	r2 := NewHub(r.st, r.log, r.relay)
	second, _, _ := r2.Wait(ctx, 1, 0)
	if first == second || first == "" {
		t.Error("a restart must show as a new epoch")
	}
}

// ---- Across a link ----

type fakeLinker struct {
	mu        sync.Mutex
	invited   []string
	ended     []string
	joined    []string
	signals   []string
	finish    []string
	joinErr   error
	inviteErr error
	creds     *relay.Credentials
	why       string
}

func (f *fakeLinker) Changed() {}
func (f *fakeLinker) Invite(link, invite, from, to, sealed string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invited = append(f.invited, to)
	return f.inviteErr
}
func (f *fakeLinker) InviteEnded(link, invite, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, reason)
}
func (f *fakeLinker) Join(link, invite string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = append(f.joined, invite)
	return f.joinErr
}
func (f *fakeLinker) Signal(link, session, sealed string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sealed)
}
func (f *fakeLinker) End(link, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finish = append(f.finish, session)
}
func (f *fakeLinker) Relay(link, session string) (*relay.Credentials, string) {
	return f.creds, f.why
}
func (f *fakeLinker) count(list *[]string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(*list)
}

func linkedRig(t *testing.T) (*hubRig, *fakeLinker, string, string) {
	r := newHubRig(t)
	maya := r.console(1, "maya")
	other := r.console(2, "other")
	r.st.Update(func(st *store.State) error {
		st.Links = []store.Link{{ID: "l1", Number: 1, Name: "Riley's", Pairs: []store.Pair{{ID: "p1", Ours: maya, Theirs: "june-there", TheirName: "june", TheirKey: []byte{9}, TheirStream: 1}}}}
		return nil
	})
	f := &fakeLinker{}
	r.h.SetLinker(f)
	return r, f, maya, other
}

func TestAPairShowsOnlyForItsAccount(t *testing.T) {
	r, _, _, _ := linkedRig(t)
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline})
	list, _ := r.h.Friends(1)
	if len(list) != 1 || list[0].ID != "p:p1" || list[0].Username != "june" || list[0].Server != "Riley's" || list[0].Status != StatusOnline {
		t.Errorf("%+v", list)
	}
	if list, _ := r.h.Friends(2); len(list) != 0 {
		t.Errorf("nobody else on either server is shown: %+v", list)
	}
}

func TestAnythingAboutSomeoneOutsideAPairIsDropped(t *testing.T) {
	r, _, _, _ := linkedRig(t)
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline, "stranger": StatusOnline})
	r.h.mu.Lock()
	_, known := r.h.remotes["l1"]["stranger"]
	r.h.mu.Unlock()
	if known {
		t.Error("kept what a link said about someone not in a pair")
	}
}

func TestALinkThatFellBehindShowsOffline(t *testing.T) {
	r, _, _, _ := linkedRig(t)
	r.h.RemoteStatus("l1", true, map[string]string{"june-there": StatusOnline})
	list, _ := r.h.Friends(1)
	if list[0].Status != StatusOffline {
		t.Errorf("%+v", list)
	}
}

func TestInviteAcrossALink(t *testing.T) {
	r, f, maya, _ := linkedRig(t)
	if _, err := r.h.Invite(1, newInviteID(), "p:p1", "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("they are not known Online: %v", err)
	}
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline})
	if _, err := r.h.Invite(2, newInviteID(), "p:p1", "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("another account cannot use that pair: %v", err)
	}
	id, err := r.h.Invite(1, newInviteID(), "p:p1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if f.count(&f.invited) != 1 {
		t.Fatal("the other server hears")
	}
	if r.h.StatusOf(maya) != StatusLinkedUp {
		t.Error("the inviter is Linked up")
	}
	if err := r.h.RemoteJoin("wrong-link", id); err == nil {
		t.Error("another link cannot join it")
	}
	if err := r.h.RemoteJoin("l1", id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(1), "joined", "") {
		t.Error("the inviter hears")
	}
	r.h.RemoteSignal("l1", id, "answer")
	if e := r.events(1); len(e) != 1 || e[0].Sealed != "answer" {
		t.Errorf("%+v", e)
	}
	r.h.Signal(1, id, "offer")
	waitTrue(t, func() bool { return f.count(&f.signals) == 1 })
	r.h.RemoteEnd("l1", id)
	if !hasEvent(r.events(1), "session_ended", "") {
		t.Error("the inviter hears it ended")
	}
}

func waitTrue(t *testing.T, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("it never happened")
}

func TestInviteFromALink(t *testing.T) {
	i1 := newInviteID()
	r, f, maya, other := linkedRig(t)
	if err := r.h.RemoteInvite("l1", i1, "stranger", maya, "s"); err == nil {
		t.Error("someone not in a pair cannot knock")
	}
	if err := r.h.RemoteInvite("l1", i1, "june-there", other, "s"); err == nil {
		t.Error("nor knock on someone else")
	}
	if err := r.h.RemoteInvite("nope", i1, "june-there", maya, "s"); err == nil {
		t.Error("nor from another link")
	}
	r.h.SetStatus(1, StatusOffline, false)
	if err := r.h.RemoteInvite("l1", i1, "june-there", maya, "s"); err == nil {
		t.Error("nor on someone Offline")
	}
	r.h.SetStatus(1, StatusOnline, false)
	r.events(1)
	if err := r.h.RemoteInvite("l1", i1, "june-there", maya, "s"); err != nil {
		t.Fatal(err)
	}
	var seen Event
	for _, e := range r.events(1) {
		if e.Type == "invite" {
			seen = e
		}
	}
	if seen.Invite != i1 || seen.From != "p:p1" || seen.Sealed != "s" {
		t.Errorf("%+v", seen)
	}
	list, _ := r.h.Friends(1)
	if list[0].Invite == nil || list[0].Invite.ID != i1 {
		t.Errorf("%+v", list[0])
	}
	if _, err := r.h.Join(1, i1); err != nil {
		t.Fatal(err)
	}
	if f.count(&f.joined) != 1 {
		t.Error("the inviter's server decides")
	}
	if r.h.StatusOf(maya) != StatusLinkedUp {
		t.Error("Linked up")
	}
	r.h.End(1, i1)
	waitTrue(t, func() bool { return f.count(&f.finish) == 1 })
}

func TestJoinRefusedByTheInvitersServer(t *testing.T) {
	i1 := newInviteID()
	r, f, maya, _ := linkedRig(t)
	r.h.RemoteInvite("l1", i1, "june-there", maya, "s")
	f.joinErr = ErrUnavailable
	if _, err := r.h.Join(1, i1); !errors.Is(err, ErrUnavailable) {
		t.Error(err)
	}
	if r.h.StatusOf(maya) != StatusOnline {
		t.Error("not joined")
	}
}

func TestRemoteInviteEnding(t *testing.T) {
	i1 := newInviteID()
	r, f, maya, _ := linkedRig(t)
	r.h.RemoteInvite("l1", i1, "june-there", maya, "s")
	r.events(1)
	r.h.RemoteInviteEnded("other-link", i1, EndCancelled)
	if _, known := r.h.invites[i1]; !known {
		t.Error("another link cannot end it")
	}
	r.h.RemoteInviteEnded("l1", i1, EndCancelled)
	if !hasEvent(r.events(1), "invite_ended", EndCancelled) {
		t.Error("the friend hears")
	}
	i2 := newInviteID()
	r.h.RemoteInvite("l1", i2, "june-there", maya, "s")
	r.h.Decline(1, i2)
	waitTrue(t, func() bool { return f.count(&f.ended) == 1 })
}

func TestRelayOrderAcrossALink(t *testing.T) {
	i1 := newInviteID()
	r, f, maya, _ := linkedRig(t)
	r.h.RemoteInvite("l1", i1, "june-there", maya, "s")
	r.h.Join(1, i1)

	// The friend's side: the host's server's relay first.
	f.creds = &relay.Credentials{Username: "theirs"}
	creds, _, _ := r.h.Relay(1, i1)
	if creds == nil || creds.Username != "theirs" {
		t.Errorf("%+v", creds)
	}
	// The host's has none: the friend's own.
	f.creds, f.why = nil, "off"
	creds, _, _ = r.h.Relay(1, i1)
	if creds == nil || creds.Username != "u."+i1 {
		t.Errorf("%+v", creds)
	}
}

func TestRelayForALinkOnlyForAGameWithItsPeople(t *testing.T) {
	i1 := newInviteID()
	r, _, maya, _ := linkedRig(t)
	if creds, _ := r.h.RemoteRelay("l1", "made-up"); creds != nil {
		t.Error("a made-up session")
	}
	r.h.RemoteInvite("l1", i1, "june-there", maya, "s")
	if creds, _ := r.h.RemoteRelay("l1", i1); creds != nil {
		t.Error("not before a join")
	}
	r.h.Join(1, i1)
	if creds, _ := r.h.RemoteRelay("other", i1); creds != nil {
		t.Error("another link")
	}
	if creds, _ := r.h.RemoteRelay("l1", i1); creds == nil {
		t.Error("a real session may have it")
	}
}

func TestInviterPicksTheIDAndARetryIsSafe(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, b)
	r.friends(c, b)
	id := newInviteID()
	if _, err := r.h.Invite(1, "not-hex", b, "s"); !errors.Is(err, ErrBadRequest) {
		t.Error(err)
	}
	got, err := r.h.Invite(1, id, b, "s")
	if err != nil || got != id {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := r.h.Invite(1, id, b, "s"); err != nil || got != id {
		t.Errorf("a retry after a lost answer: %v %v", got, err)
	}
	if _, err := r.h.Invite(1, id, b, "different"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a repeat with a different box: %v", err)
	}
	if _, err := r.h.Invite(3, id, b, "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("someone else cannot take that id: %v", err)
	}
}

func TestTooManyKnocks(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	for i := range knocksAllowed {
		id, err := r.h.Invite(1, newInviteID(), b, "s")
		if err != nil {
			t.Fatalf("knock %d: %v", i, err)
		}
		r.h.Cancel(1, id)
	}
	if _, err := r.h.Invite(1, newInviteID(), b, "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("too many: %v", err)
	}
	r.advance(knocksWindow + time.Minute)
	r.h.Seen(1)
	r.h.Seen(2)
	if _, err := r.h.Invite(1, newInviteID(), b, "s"); err != nil {
		t.Errorf("later it is fine: %v", err)
	}
}

func TestBoxSizes(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	big := strings.Repeat("x", maxInviteBox+1)
	if _, err := r.h.Invite(1, newInviteID(), b, big); !errors.Is(err, ErrBadRequest) {
		t.Error(err)
	}
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	if err := r.h.Signal(1, id, strings.Repeat("x", maxSignalBox+1)); !errors.Is(err, ErrBadRequest) {
		t.Error(err)
	}
}

func TestWaitAnswerIsCapped(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	r.events(2)
	box := strings.Repeat("x", maxSignalBox)
	for range 20 {
		if err := r.h.Signal(1, id, box); err != nil {
			t.Fatal(err)
		}
	}
	_, first, next := r.h.Wait(context.Background(), 2, 0)
	if len(first) == 0 || len(first) == 20 {
		t.Fatalf("%d events", len(first))
	}
	if next != first[len(first)-1].Seq {
		t.Errorf("next %d, last %d", next, first[len(first)-1].Seq)
	}
	_, second, _ := r.h.Wait(context.Background(), 2, next)
	if len(second) == 0 || second[0].Seq != next+1 {
		t.Errorf("it must carry on where it stopped: %+v", second[:1])
	}
}

func TestASessionEndsIfItsConsoleGoesAway(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	r.advance(sessionSilence - time.Minute)
	r.h.Seen(1)
	r.h.Seen(2)
	r.h.Tick()
	if r.h.invites[id] == nil {
		t.Fatal("too soon")
	}
	r.advance(sessionSilence + time.Minute)
	r.h.Seen(1)
	r.h.Tick()
	if r.h.invites[id] != nil {
		t.Error("the session should have ended")
	}
	if len(r.relay.ended) == 0 {
		t.Error("and its relay")
	}
	r.h.Seen(2)
	if statusOf(t, r.h, 1, b) != StatusOnline {
		t.Error("they can be knocked on again")
	}
}

func TestASessionHasALongestTime(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	for range 30 {
		r.advance(time.Hour)
		r.h.Seen(1)
		r.h.Seen(2)
		r.h.Tick()
	}
	if r.h.invites[id] != nil {
		t.Error("24 hours is the most")
	}
}

func TestAKnockFromALinkEndsHereToo(t *testing.T) {
	r, _, maya, _ := linkedRig(t)
	id := newInviteID()
	if err := r.h.RemoteInvite("l1", id, "june-there", maya, "s"); err != nil {
		t.Fatal(err)
	}
	r.events(1)
	r.advance(inviteLife + time.Minute)
	r.h.Seen(1)
	r.h.Tick()
	if r.h.invites[id] != nil {
		t.Error("a linked server that went away must not leave it on the list")
	}
	if !hasEvent(r.events(1), "invite_ended", EndTimeout) {
		t.Error("the friend hears")
	}
}

func TestRemovingAFriendEndsWhatTheyHaveOpen(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	id, _ := r.h.Invite(1, newInviteID(), b, "s")
	r.events(2)
	r.h.EndBetween(a, b)
	if !hasEvent(r.events(2), "invite_ended", EndUnavailable) {
		t.Error("the knock goes")
	}
	if r.h.invites[id] != nil {
		t.Error("gone")
	}
	id, _ = r.h.Invite(1, newInviteID(), b, "s")
	r.h.Join(2, id)
	r.h.EndBetween(b, a)
	if r.h.invites[id] != nil || len(r.relay.ended) < 2 {
		t.Error("a session ends on the server, and its relay")
	}
}

func TestUnlinkingEndsWhatTheLinkHasOpen(t *testing.T) {
	r, _, maya, _ := linkedRig(t)
	id := newInviteID()
	r.h.RemoteInvite("l1", id, "june-there", maya, "s")
	r.h.Join(1, id)
	r.h.EndLink("l1")
	if r.h.invites[id] != nil || len(r.relay.ended) == 0 {
		t.Error("it should end, with its relay")
	}
}

func TestAJoinRetryAfterALostAnswer(t *testing.T) {
	r, _, _, _ := linkedRig(t)
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline})
	id, err := r.h.Invite(1, newInviteID(), "p:p1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.h.RemoteJoin("l1", id); err != nil {
		t.Fatal(err)
	}
	if err := r.h.RemoteJoin("l1", id); err != nil {
		t.Errorf("the same join again is fine: %v", err)
	}
}

func TestOneInviteOutAcrossALinkHoweverFast(t *testing.T) {
	r, _, _, _ := linkedRig(t)
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.h.Invite(1, newInviteID(), "p:p1", "s"); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d invites accepted", won)
	}
}

func TestAnInviteWithAFailedCallIsNotLeftBehind(t *testing.T) {
	r, f, _, _ := linkedRig(t)
	f.inviteErr = ErrUnavailable
	r.h.RemoteStatus("l1", false, map[string]string{"june-there": StatusOnline})
	if _, err := r.h.Invite(1, newInviteID(), "p:p1", "s"); err == nil {
		t.Error("it was refused")
	}
	if len(r.h.invites) != 0 {
		t.Error("left behind")
	}
}

func TestAReasonFromALinkIsOneOfTheWords(t *testing.T) {
	r, _, maya, _ := linkedRig(t)
	id := newInviteID()
	r.h.RemoteInvite("l1", id, "june-there", maya, "s")
	r.events(1)
	r.h.RemoteInviteEnded("l1", id, "click here")
	if !hasEvent(r.events(1), "invite_ended", EndUnavailable) {
		t.Error("anything else becomes unavailable")
	}
}

func TestListChangedIsSaidOnceUntilHeard(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	r.events(1)
	for range 50 {
		r.h.SetStatus(2, StatusOffline, false)
		r.h.SetStatus(2, StatusOnline, false)
	}
	if got := r.events(1); len(got) > 2 {
		t.Errorf("%d events for one change of list", len(got))
	}
}

func TestASessionHasABudget(t *testing.T) {
	var s session
	now := time.Unix(1000, 0)
	for i := range requestsPerSecond {
		if !s.withinBudget(now) {
			t.Fatalf("request %d refused", i)
		}
	}
	if s.withinBudget(now) {
		t.Error("past the budget")
	}
	if !s.withinBudget(now.Add(time.Second)) {
		t.Error("a new second starts over")
	}
}

func TestSigningInAgainDoesNotSave(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "a")
	var before, after store.State
	r.st.ReadJSON("state.json", &before)
	before.Games.Direct = 12345
	r.st.WriteJSON("state.json", &before)
	r.h.Signin(1, "a", "a", StatusOnline)
	r.st.ReadJSON("state.json", &after)
	if after.Games.Direct != 12345 {
		t.Error("nothing changed, so nothing should be written")
	}
}

// ---- One person on more than one console ----

// join pairs a new console with an invite made for someone, as a console using the code does, and
// signs that account in on it.
func (r *hubRig) join(number int, forPerson, account string) string {
	r.t.Helper()
	code := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: forPerson, Expires: r.clock.Add(time.Hour)}
	r.st.Update(func(st *store.State) error {
		st.Codes = append(st.Codes, code)
		st.NextConsole = number
		return nil
	})
	if res, err := r.h.joinConsole(code, []byte{byte(number)}, consoleJoining{Account: account, Username: account}); err != nil || res.Console != number {
		r.t.Fatalf("%+v %v", res, err)
	}
	r.h.Connected(number, []byte{byte(number)}, 1)
	id, _, err := r.h.Signin(number, account, account, StatusOnline)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func keyOf(t *testing.T, h *Hub, number int, friend string) []byte {
	t.Helper()
	list, _ := h.Friends(number)
	for _, f := range list {
		if f.ID == friend {
			return f.Key
		}
	}
	t.Fatalf("%s is not on the list", friend)
	return nil
}

func hasChosen(events []Event, status string) bool {
	for _, e := range events {
		if e.Type == "chosen" && e.Status == status {
			return true
		}
	}
	return false
}

// me on consoles 1 and 2, 2 used last, and a friend on console 3.
func twoConsoles(t *testing.T) (*hubRig, string, string) {
	r := newHubRig(t)
	me := r.console(1, "me")
	friend := r.console(3, "friend")
	r.friends(me, friend)
	r.advance(time.Second)
	if again := r.join(2, me, "me"); again != me {
		t.Fatalf("console 2 made a second me: %s", again)
	}
	r.events(1)
	r.events(2)
	r.events(3)
	return r, me, friend
}

func TestAnInviteForSomeoneUsedByThemAddsTheirConsole(t *testing.T) {
	r, me, friend := twoConsoles(t)
	st := r.st.Snapshot()
	if p := st.Person(me); !slices.Equal(p.Consoles(), []int{2, 1}) || len(st.People) != 2 || st.AreFriends(me, me) {
		t.Errorf("%+v", st.People)
	}
	if list, _ := r.h.Friends(3); len(list) != 1 || list[0].ID != me {
		t.Errorf("the friend sees me once: %+v", list)
	}
	for _, n := range []int{1, 2} {
		if list, _ := r.h.Friends(n); len(list) != 1 || list[0].ID != friend {
			t.Errorf("console %d: never in my own list: %+v", n, list)
		}
	}
}

// Console 2 came in with me, so it knows everyone already on my consoles.
func TestAJoinedConsoleKnowsWhoIsOnTheOthers(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	kid, _, _ := r.h.Signin(1, "kid", "kid", StatusOnline)
	r.h.Signin(1, "me", "me", StatusOnline)
	r.join(2, me, "me")
	if again, _, _ := r.h.Signin(2, "kid", "kid", StatusOnline); again != kid {
		t.Errorf("the kid on console 2 is someone new")
	}
}

// An account is not a secret, so an invite used by anyone else only brings in new people.
func TestAnInviteUsedBySomeoneElseOnlyBringsNewPeople(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	danny := r.join(3, me, "danny")
	st := r.st.Snapshot()
	if danny == me || !st.AreFriends(me, danny) || st.Person(me).On(3) {
		t.Errorf("%+v", st.People)
	}
	if other, _, _ := r.h.Signin(3, "me", "me", StatusOnline); other == me {
		t.Error("my account on Danny's console is not me")
	}
}

func TestAKnockRingsTheConsoleUsedLast(t *testing.T) {
	r, me, _ := twoConsoles(t)
	if key := keyOf(t, r.h, 3, me); !slices.Equal(key, []byte{2}) {
		t.Errorf("the friend is given console 2's key: %v", key)
	}
	id, err := r.h.Invite(3, newInviteID(), me, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(2), "invite", "") || hasEvent(r.events(1), "invite", "") {
		t.Error("it rings console 2 only")
	}
	if list, _ := r.h.Friends(1); list[0].Invite != nil {
		t.Error("console 1 shows no knock")
	}
	if list, _ := r.h.Friends(2); list[0].Invite == nil {
		t.Error("console 2 does")
	}
	if _, err := r.h.Join(1, id); err == nil {
		t.Error("only the console it rang can answer")
	}
	if _, err := r.h.Join(2, id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(3), "joined", "") {
		t.Error("the friend hears")
	}
}

// Of the consoles showing me Online, a knock rings the one I came back to last, which is then the
// one used last.
func TestComingBackToAConsoleMovesTheKnock(t *testing.T) {
	r, me, _ := twoConsoles(t)
	r.h.SetStatus(2, StatusOffline, false)
	if statusOf(t, r.h, 3, me) != StatusOnline || !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) {
		t.Error("console 1 still shows me, so it rings there")
	}
	r.h.SetStatus(1, StatusOffline, false)
	if statusOf(t, r.h, 3, me) != StatusOffline {
		t.Error("both are Offline")
	}
	r.advance(time.Second)
	r.h.SetStatus(1, StatusOnline, false)
	st := r.st.Snapshot()
	if !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) || st.Person(me).Console != 1 {
		t.Error("back on console 1, it is the one used last")
	}
}

// The friend opens my box with the key on their list, so while I have an invite out, they are given
// the key of the console it came from.
func TestAnInviteFromTheOtherConsoleGivesItsKey(t *testing.T) {
	r, _, friend := twoConsoles(t)
	id, err := r.h.Invite(1, newInviteID(), friend, "s")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := r.h.Friends(3)
	if !slices.Equal(list[0].Key, []byte{1}) || list[0].Invite == nil || list[0].Invite.ID != id {
		t.Errorf("%+v", list[0])
	}
	if err := r.h.Cancel(2, id); err == nil {
		t.Error("only the console it came from can take it back")
	}
	r.h.Signout(2)
	if r.h.invites[id] == nil {
		t.Error("leaving the other console does not take it back")
	}
}

func TestOfflineChosenOnOneConsoleHoldsOnAll(t *testing.T) {
	r, me, _ := twoConsoles(t)
	r.h.SetStatus(1, StatusOffline, true)
	if statusOf(t, r.h, 3, me) != StatusOffline {
		t.Error("Offline, though console 2 is on")
	}
	if !hasChosen(r.events(2), StatusOffline) || hasChosen(r.events(1), StatusOffline) {
		t.Error("console 2 hears it, console 1 said it")
	}
	_, chose, _ := r.h.Signin(2, "me", "me", StatusOnline)
	if st := r.st.Snapshot(); !chose || !st.Person(me).ChoseOffline {
		t.Error("a console signing in is told, and it is saved")
	}
	if _, err := r.h.Invite(3, newInviteID(), me, "s"); err == nil {
		t.Error("nobody can knock")
	}
	r.h.SetStatus(2, StatusOffline, false)
	r.h.SetStatus(2, StatusOnline, true)
	if statusOf(t, r.h, 3, me) != StatusOnline || !hasChosen(r.events(1), StatusOnline) {
		t.Error("Online chosen on console 2 holds too")
	}
	r.h.SetStatus(2, StatusOffline, false)
	if st := r.st.Snapshot(); st.Person(me).ChoseOffline {
		t.Error("a console's own Offline is not a choice")
	}
}

// Only that console goes: what I have open on another carries on.
func TestRemovingOneOfTheirConsoles(t *testing.T) {
	r, me, _ := twoConsoles(t)
	id, _ := r.h.Invite(3, newInviteID(), me, "s")
	r.h.Join(2, id)
	r.st.Update(func(st *store.State) error { st.RemoveConsole(1); return nil })
	r.h.ConsoleRemoved(1)
	if !hasEvent(r.events(1), "removed", "") || r.h.invites[id] == nil || r.h.StatusOf(me) != StatusLinkedUp {
		t.Error("the game on console 2 carries on")
	}
	r.st.Update(func(st *store.State) error { st.RemoveConsole(2); return nil })
	r.h.ConsoleRemoved(2)
	if r.h.invites[id] != nil || len(r.relay.ended) == 0 {
		t.Error("removing console 2 ends it")
	}
}

// A knock ringing a console that is taken off ends, and its inviter hears.
func TestAKnockOnARemovedConsoleIsUnavailable(t *testing.T) {
	r, me, _ := twoConsoles(t)
	id, _ := r.h.Invite(3, newInviteID(), me, "s")
	r.h.ConsoleRemoved(2)
	if r.h.invites[id] != nil || !hasEvent(r.events(3), "invite_ended", EndUnavailable) {
		t.Error("the inviter hears it is unavailable")
	}
}

func TestLeavingOneOfTheirConsoles(t *testing.T) {
	r, me, friend := twoConsoles(t)
	if err := r.h.Leave(2); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if p := st.Person(me); p == nil || !slices.Equal(p.Consoles(), []int{1}) || !st.AreFriends(me, friend) {
		t.Errorf("%+v", st.People)
	}
	if !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) {
		t.Error("the friend is given console 1's key")
	}
}

func TestAKnockFromALinkRingsTheConsoleUsedLast(t *testing.T) {
	r, _, maya, _ := linkedRig(t)
	r.advance(time.Second)
	r.join(3, maya, "maya")
	r.events(1)
	r.events(3)
	if r.h.KeyConsole(maya) != 3 {
		t.Error("the linked server hears console 3's key")
	}
	id := newInviteID()
	if err := r.h.RemoteInvite("l1", id, "june-there", maya, "s"); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(3), "invite", "") || hasEvent(r.events(1), "invite", "") {
		t.Error("it rings console 3 only")
	}
	if _, err := r.h.Join(3, id); err != nil {
		t.Fatal(err)
	}
}

// pairWith uses a code on a console, as a console pairing does.
func (r *hubRig) pairWith(code store.Code, number int, account string) error {
	r.st.Update(func(st *store.State) error {
		if st.Code(code.ID) == nil {
			st.Codes = append(st.Codes, code)
		}
		if st.ConsoleByNumber(number) == nil {
			st.NextConsole = number
		}
		return nil
	})
	_, err := r.h.joinConsole(code, []byte{byte(number)}, consoleJoining{Account: account, Username: account})
	return err
}

// A person's code adds any of their consoles, as often as they like, and nobody else's.
func TestAPersonsCodeAddsEachOfTheirConsoles(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	code := store.Code{ID: store.NewID(), Kind: store.CodePerson, For: me}
	for _, n := range []int{2, 3} {
		if err := r.pairWith(code, n, "me"); err != nil {
			t.Fatal(err)
		}
	}
	st := r.st.Snapshot()
	if p := st.Person(me); !slices.Equal(p.Consoles(), []int{3, 2, 1}) || len(st.People) != 1 || st.Code(code.ID) == nil {
		t.Errorf("one person on three consoles, the code kept: %+v", st)
	}
	if err := r.pairWith(code, 4, "someone"); err == nil {
		t.Error("another account is refused")
	}
	if st := r.st.Snapshot(); st.ConsoleByNumber(4) != nil || len(st.People) != 1 {
		t.Errorf("and nothing is made for it: %+v", st)
	}
}

// After Change code, only the new code brings them back on a console that was signed out.
func TestChangeCodeSignsThemOutUntilTheNewCode(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	friend := r.console(3, "friend")
	r.friends(me, friend)
	r.st.Update(func(st *store.State) error { st.SignOut(me); return nil })
	r.h.ConsoleRemoved(1)
	invite := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: friend, Expires: r.clock.Add(time.Hour)}
	if err := r.pairWith(invite, 1, "me"); err != nil {
		t.Fatal(err)
	}
	if st := r.st.Snapshot(); st.Person(me).On(1) {
		t.Error("someone else's invite does not bring them back")
	}
	r.st.Update(func(st *store.State) error {
		st.Consoles[0].Removed = true
		st.People = slices.DeleteFunc(st.People, func(p store.Person) bool { return p.ID != me && p.ID != friend })
		return nil
	})
	code := store.Code{ID: store.NewID(), Kind: store.CodePerson, For: me}
	if err := r.pairWith(code, 1, "me"); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if !st.Person(me).On(1) || st.ConsoleByNumber(1).Removed || !st.AreFriends(me, friend) {
		t.Errorf("back with their friends: %+v", st)
	}
}

// The invite someone used is their code from then on: the same letters, on each of their consoles,
// with no expiry, and for nobody else.
func TestTheInviteTheyUsedIsTheirCode(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	invite := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: me, Note: "Danny", Expires: r.clock.Add(time.Hour)}
	if err := r.pairWith(invite, 3, "danny"); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	danny := st.PersonOn(3, "danny")
	c := st.Code(invite.ID)
	if danny == nil || c == nil || c.Kind != store.CodePerson || c.For != danny.ID || !c.Expires.IsZero() || !st.AreFriends(me, danny.ID) {
		t.Fatalf("%+v", st)
	}
	if err := r.pairWith(*c, 4, "danny"); err != nil {
		t.Fatal(err)
	}
	if err := r.pairWith(*c, 5, "someone"); err == nil {
		t.Error("nobody else can use it")
	}
	st = r.st.Snapshot()
	if p := st.Person(danny.ID); !slices.Equal(p.Consoles(), []int{4, 3}) || len(st.People) != 2 {
		t.Errorf("Danny on both his consoles: %+v", st.People)
	}
}
