package console

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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
