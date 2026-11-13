package console

import (
	"errors"
	"testing"
	"time"
)

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
	other, _, _ := r.h.Signin(2, "other", "other", StatusOnline, false)
	r.friends(a, other)
	r.h.Signin(2, "b", "b", StatusOnline, false)
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
