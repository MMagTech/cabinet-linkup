package console

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/relay"
)

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
