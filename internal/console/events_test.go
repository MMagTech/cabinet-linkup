package console

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
