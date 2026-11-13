package console

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

func newHandlerRig(t *testing.T) *Handler {
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
	key, err := proof.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(NewHub(st, log, &fakeRelay{}), st, log, key, nil)
}

// guess sends a message 1 that fits no code, from one source, and returns the answer's status.
func guess(h *Handler, source string) int {
	body := make([]byte, 1+pairFirstSize)
	body[0] = proof.KindPairFirst
	rand.Read(body[1:])
	r := httptest.NewRequest(http.MethodPost, proof.Path, bytes.NewReader(body))
	r.RemoteAddr = source
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestAGuesserIsRefusedOnItsOwnAndInAll(t *testing.T) {
	h := newHandlerRig(t)
	for i := range triesPerAddress {
		if got := guess(h, "203.0.113.7:4000"); got != http.StatusForbidden {
			t.Fatalf("guess %d answered %d, want 403", i+1, got)
		}
	}
	if got := guess(h, "203.0.113.7:4000"); got != http.StatusTooManyRequests {
		t.Fatalf("a source past its own limit answered %d", got)
	}
	// Behind a reverse proxy every source is private, and only the total counts.
	for i := 0; i < triesInAll-triesPerAddress; i++ {
		if got := guess(h, "172.17.0.1:4000"); got != http.StatusForbidden {
			t.Fatalf("private guess %d answered %d, want 403", i+1, got)
		}
	}
	if got := guess(h, "172.17.0.1:4000"); got != http.StatusTooManyRequests {
		t.Fatalf("past the total a guess answered %d", got)
	}
}

// Every failure is noted under the lock all consoles' requests take, so what it keeps must not grow
// with how many came before: a stranger behind a reverse proxy, or with many addresses, could
// otherwise make each failure slower than the last.
func TestFailedPairingsKeepOnlyWhatTheLimitsNeed(t *testing.T) {
	h := newHandlerRig(t)
	for range 5000 {
		guess(h, "172.17.0.1:4000")
	}
	for i := range 2 * triesSources {
		guess(h, fmt.Sprintf("[2001:4860:%x::1]:4000", i))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.allTries) > triesInAll+1 {
		t.Errorf("the total keeps %d times", len(h.allTries))
	}
	if len(h.tries) > triesSources {
		t.Errorf("the table keeps %d sources", len(h.tries))
	}
	for _, list := range h.tries {
		if len(list) > triesPerAddress+1 {
			t.Fatalf("a source keeps %d times", len(list))
		}
	}
}

// Only hosts false marks an app that only joins: missing, as every console sends it, hosts.
func TestHostsFalseIsTheOnlyJoinsOnly(t *testing.T) {
	for raw, want := range map[string]bool{`{}`: false, `{"hosts":true}`: false, `{"hosts":false}`: true, `{"hosts":null}`: false} {
		var q request
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			t.Fatal(err)
		}
		var j consoleJoining
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			t.Fatal(err)
		}
		if joinsOnly(q.Hosts) != want || joinsOnly(j.Hosts) != want {
			t.Errorf("%s: want %v", raw, want)
		}
	}
}
