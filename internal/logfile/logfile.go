// Package logfile keeps the small log in the data folder, which survives an update
// where Docker's own log does not. It is written only when something happens, capped
// at about a megabyte with the oldest lines dropping off, and an owner can paste it into
// a public issue.
//
// So it never records codes, passwords, keys, internet addresses, usernames, game titles
// or traffic. That is held by what this package takes: numbers and the fixed words
// below, and no way to add a free string. Nothing from the network can reach a line.
package logfile

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/store"
)

// The size it is held to, and what is left when it goes over.
const (
	maxSize  = 1 << 20
	keepSize = 3 << 18
)

// Reason is why a game failed, one of the words a console may report.
type Reason int

// The reasons a game can fail. PROTOCOL.md has the words a console sends.
const (
	ReasonOther Reason = iota
	ReasonNoPath
	ReasonTimeout
	ReasonRelayLimit
	ReasonBehind
)

// ReasonFromWord turns a console's word into a Reason. Anything unknown is ReasonOther,
// so what a console sends never reaches a line.
func ReasonFromWord(word string) Reason {
	switch word {
	case "no_path":
		return ReasonNoPath
	case "timeout":
		return ReasonTimeout
	case "relay_limit":
		return ReasonRelayLimit
	case "behind":
		return ReasonBehind
	}
	return ReasonOther
}

func (r Reason) String() string {
	switch r {
	case ReasonNoPath:
		return "no path found"
	case ReasonTimeout:
		return "timed out"
	case ReasonRelayLimit:
		return "relay limit reached"
	case ReasonBehind:
		return "a console or server is behind"
	}
	return "other"
}

// Problem is something wrong with the server itself.
type Problem string

// The problems the server can log.
const (
	ProblemSave      Problem = "the data folder could not be written"
	ProblemRelayPort Problem = "the relay's port could not be opened"
	ProblemRelayAddr Problem = "the relay's address could not be found"
	ProblemInternal  Problem = "something failed inside the server"
)

// Line is one line of the log.
type Line struct {
	At   string
	Line string
}

// Log is the capped log file. It also copies each line to out, for Docker's log.
type Log struct {
	mu     sync.Mutex
	path   string
	out    io.Writer
	size   int64
	failed int // failed pairing attempts not yet written
	now    func() time.Time
}

// Open opens the log file, keeping what it already holds.
func Open(path string, out io.Writer) (*Log, error) {
	l := &Log{path: path, out: out, now: time.Now}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	l.size = info.Size()
	return l, nil
}

// Path is the file itself, for the page's download.
func (l *Log) Path() string {
	return l.path
}

var versionWord = regexp.MustCompile(`[^0-9A-Za-z.+-]`)

// Started records the server starting, and its version.
func (l *Log) Started(version string) {
	l.write("server started, version " + versionWord.ReplaceAllString(version, ""))
}

// Stopped records the server stopping.
func (l *Log) Stopped() { l.write("server stopped") }

// ConsolePaired records a console pairing.
func (l *Log) ConsolePaired(number int) { l.write(console(number) + " paired") }

// ConsoleRemoved records a console being removed or leaving.
func (l *Log) ConsoleRemoved(number int) { l.write(console(number) + " removed") }

// LinkAdded records two servers being linked.
func (l *Log) LinkAdded(number int) { l.write(link(number) + " added") }

// LinkRemoved records a link being removed.
func (l *Log) LinkRemoved(number int) { l.write(link(number) + " removed") }

// LinkUp records a link answering again.
func (l *Log) LinkUp(number int) { l.write(link(number) + " up") }

// LinkDown records a link no longer answering.
func (l *Log) LinkDown(number int) { l.write(link(number) + " down") }

// RelayOn records the relay turning on.
func (l *Log) RelayOn() { l.write("relay on") }

// RelayOff records the relay turning off.
func (l *Log) RelayOff() { l.write("relay off") }

// RelayLimit records the relay reaching its monthly limit.
func (l *Log) RelayLimit() { l.write("relay limit reached") }

// RelayTest records the relay's self-check.
func (l *Log) RelayTest(passed bool) {
	if passed {
		l.write("relay test passed")
	} else {
		l.write("relay test failed")
	}
}

// GameDirect records a game that connected directly.
func (l *Log) GameDirect() { l.write("game connected, direct") }

// GameRelayed records a game that connected through a relay.
func (l *Log) GameRelayed() { l.write("game connected, relay") }

// GameFailed records a game that could not connect.
func (l *Log) GameFailed(r Reason) { l.write("game failed: " + r.String()) }

// Problem records something wrong with the server itself.
func (l *Log) Problem(p Problem) { l.write(string(p)) }

// PairFailed counts a failed or rate-limited pairing attempt. Counts go in the log, by
// FlushCounts, and never who tried.
func (l *Log) PairFailed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed++
}

// FlushCounts writes the counts gathered since the last call.
func (l *Log) FlushCounts() {
	l.mu.Lock()
	n := l.failed
	l.failed = 0
	l.mu.Unlock()
	if n == 1 {
		l.write("1 failed pairing attempt")
	} else if n > 1 {
		l.write(strconv.Itoa(n) + " failed pairing attempts")
	}
}

// Tail returns the last n lines, newest first.
func (l *Log) Tail(n int) []Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	rows := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []Line
	for i := len(rows) - 1; i >= 0 && len(out) < n; i-- {
		at, text, ok := strings.Cut(rows[i], "  ")
		if ok {
			out = append(out, Line{At: at, Line: text})
		}
	}
	return out
}

func console(n int) string { return "console " + strconv.Itoa(n) }
func link(n int) string    { return "link " + strconv.Itoa(n) }

func (l *Log) write(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row := fmt.Sprintf("%s  %s\n", l.now().UTC().Format(time.RFC3339), text)
	if l.out != nil {
		fmt.Fprint(l.out, row)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	n, _ := f.WriteString(row)
	f.Close()
	l.size += int64(n)
	if l.size > maxSize {
		l.trim()
	}
}

// trim keeps the newest lines. A failure leaves the file as it was, over the cap until
// the next try, which is better than losing it.
func (l *Log) trim() {
	data, err := os.ReadFile(l.path)
	if err != nil || len(data) <= keepSize {
		return
	}
	kept := data[len(data)-keepSize:]
	if i := strings.IndexByte(string(kept), '\n'); i >= 0 {
		kept = kept[i+1:]
	}
	if store.WriteFile(l.path, kept) == nil {
		l.size = int64(len(kept))
	}
}
