package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "log.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLinesAndOrder(t *testing.T) {
	l := newLog(t)
	l.now = func() time.Time { return time.Date(2026, 11, 5, 1, 20, 0, 0, time.UTC) }
	l.Started("1.0.0")
	l.ConsolePaired(4)
	l.LinkUp(1)
	l.GameDirect()
	l.GameFailed(ReasonNoPath)
	got := l.Tail(10)
	if len(got) != 5 {
		t.Fatalf("%d lines", len(got))
	}
	if got[0].Line != "game failed: no path found" || got[4].Line != "server started, version 1.0.0" {
		t.Errorf("newest first: %+v", got)
	}
	if got[0].At != "2026-11-05T01:20:00Z" {
		t.Errorf("time: %s", got[0].At)
	}
	if got[3].Line != "console 4 paired" {
		t.Errorf("a console shows as a number: %+v", got[3])
	}
}

func TestTailLimit(t *testing.T) {
	l := newLog(t)
	for range 250 {
		l.GameDirect()
	}
	if n := len(l.Tail(200)); n != 200 {
		t.Errorf("%d lines", n)
	}
}

func TestCapDropsOldest(t *testing.T) {
	l := newLog(t)
	l.Started("first")
	for range 1 << 16 {
		l.GameDirect()
	}
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxSize {
		t.Errorf("%d bytes is over the cap", info.Size())
	}
	data, _ := os.ReadFile(l.Path())
	if strings.Contains(string(data), "first") {
		t.Error("the oldest line should have dropped off")
	}
	if !strings.HasSuffix(string(data), "game connected, direct\n") {
		t.Error("the newest line must stay whole")
	}
	if !strings.Contains(strings.SplitN(string(data), "\n", 2)[0], "game connected") {
		t.Error("a cut must leave whole lines")
	}
}

func TestFailedAttemptsAreCounts(t *testing.T) {
	l := newLog(t)
	l.FlushCounts()
	if len(l.Tail(5)) != 0 {
		t.Error("nothing to write means no line")
	}
	l.PairFailed()
	l.FlushCounts()
	l.PairFailed()
	l.PairFailed()
	l.PairFailed()
	l.FlushCounts()
	got := l.Tail(5)
	if got[0].Line != "3 failed pairing attempts" || got[1].Line != "1 failed pairing attempt" {
		t.Errorf("%+v", got)
	}
}

func TestReasonsNeverCarryWhatAConsoleSent(t *testing.T) {
	if ReasonFromWord("alex at 203.0.113.5 playing Contra") != ReasonOther {
		t.Error("an unknown word must become other")
	}
	l := newLog(t)
	l.GameFailed(ReasonFromWord("alex at 203.0.113.5 playing Contra"))
	if got := l.Tail(1)[0].Line; got != "game failed: other" {
		t.Error(got)
	}
}

func TestVersionCannotCarryAnything(t *testing.T) {
	l := newLog(t)
	l.Started("1.0.0 ; alex\n2026-01-01T00:00:00Z  injected")
	got := l.Tail(5)
	if len(got) != 1 || strings.ContainsAny(got[0].Line[len("server started, version "):], " \n;") {
		t.Errorf("%+v", got)
	}
}

func TestCopiesToOut(t *testing.T) {
	var out bytes.Buffer
	l, err := Open(filepath.Join(t.TempDir(), "log.txt"), &out)
	if err != nil {
		t.Fatal(err)
	}
	l.RelayOn()
	if !strings.Contains(out.String(), "relay on") {
		t.Error(out.String())
	}
}

func TestKeepsWhatItHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	l, _ := Open(path, nil)
	l.Stopped()
	again, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	again.RelayOff()
	if n := len(again.Tail(5)); n != 2 {
		t.Errorf("%d lines", n)
	}
}
