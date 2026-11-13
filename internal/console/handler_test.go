package console

import (
	"path/filepath"
	"testing"
	"time"

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
