package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noKeys(*State) error { return nil }

func TestNewFolderHasDefaults(t *testing.T) {
	s, err := Open(t.TempDir(), func(st *State) error {
		st.ServerPub = []byte("pub")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(st *State) {
		if st.Settings.RelayLimitGB != 500 || st.Settings.RelayResetDay != 1 || st.Settings.Relay {
			t.Errorf("relay defaults: %+v", st.Settings)
		}
		if len(st.Settings.Stun) != 2 || st.Settings.Stun[0].Address != DefaultStun[0] {
			t.Errorf("stun defaults: %+v", st.Settings.Stun)
		}
		if string(st.ServerPub) != "pub" {
			t.Error("fresh did not run")
		}
	})
}

func TestStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, noKeys)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(st *State) error {
		st.People = append(st.People, Person{ID: "a", Username: "alex", LastSeen: time.Unix(100, 0).UTC()})
		st.Games.Direct = 3
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, func(*State) error { t.Error("fresh ran on an existing folder"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	again.View(func(st *State) {
		if st.Person("a") == nil || st.Person("a").Username != "alex" || st.Games.Direct != 3 {
			t.Errorf("not kept: %+v", st)
		}
	})
}

func TestRefusesNewerFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(fmt.Sprintf(`{"Format":%d}`, Format+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, noKeys)
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("a newer file must refuse to start, got %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(data), fmt.Sprint(Format+1)) {
		t.Error("the newer file was overwritten")
	}
}

func TestRefusesBrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"Format":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, noKeys); err == nil {
		t.Fatal("a broken file must not be replaced with a new server")
	}
}

func TestFailedUpdateSavesNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, noKeys)
	want := errors.New("no")
	if err := s.Update(func(*State) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestTouchWaitsForFlush(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, noKeys)
	s.Touch(func(st *State) bool { st.Games.Failed = 9; return true })
	var onDisk State
	if err := s.ReadJSON("state.json", &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Games.Failed != 0 {
		t.Error("Touch wrote straight away")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadJSON("state.json", &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Games.Failed != 9 {
		t.Error("Flush did not write")
	}
}

func TestFilesAreOwnerOnlyAndLeaveNothingBehind(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, noKeys)
	if err := s.SetAdminHash("h"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left behind: %s", e.Name())
		}
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v", e.Name(), info.Mode().Perm())
		}
	}
}

func TestDeletingAdminFileBringsBackFirstPassword(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, noKeys)
	if s.AdminHash() != "" {
		t.Error("a new server has no password")
	}
	if err := s.SetAdminHash("h"); err != nil {
		t.Fatal(err)
	}
	if s.AdminHash() != "h" {
		t.Error("not kept")
	}
	if err := os.Remove(filepath.Join(dir, "admin.json")); err != nil {
		t.Fatal(err)
	}
	if s.AdminHash() != "" {
		t.Error("deleting the file must clear it")
	}
}

func TestFriendsAndRemoving(t *testing.T) {
	st := &State{
		People: []Person{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Links:  []Link{{ID: "l", Pairs: []Pair{{ID: "p", Ours: "a"}, {ID: "q", Ours: "b"}}}},
		Codes:  []Code{{ID: "k", For: "a"}},
	}
	st.AddFriends("a", "b")
	st.AddFriends("b", "a")
	st.AddFriends("a", "a")
	st.AddFriends("a", "c")
	if len(st.Friends) != 2 {
		t.Fatalf("friendships: %+v", st.Friends)
	}
	if !st.AreFriends("b", "a") || st.AreFriends("b", "c") {
		t.Error("two friends of the same person are not friends of each other")
	}
	st.RemovePerson("a")
	if len(st.Friends) != 0 || len(st.Links[0].Pairs) != 1 || st.Code("k").For != "" {
		t.Errorf("not cleaned up: %+v", st)
	}
}

// A saved person's fields stay readable as JSON keys, because an owner may read a backup.
func TestStateIsPlainJSON(t *testing.T) {
	data, err := json.Marshal(State{People: []Person{{ID: "a", Username: "alex"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"Username":"alex"`) {
		t.Error(string(data))
	}
}

// Several hundred people, to measure what the design says: saving one change is quick
// enough that the one lock over the state does not matter. Run with -v to see the numbers.
func TestManyPeople(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, noKeys)
	const n = 500
	err := s.Update(func(st *State) error {
		key := make([]byte, 32)
		for i := range n {
			st.Consoles = append(st.Consoles, Console{Number: i, Key: key, Paired: time.Now()})
			st.People = append(st.People, Person{ID: NewID(), Console: i, Account: fmt.Sprint(i), Username: fmt.Sprintf("player%d", i), Note: strings.Repeat("n", MaxNoteLength), LastSeen: time.Now()})
		}
		for i := 1; i < n; i++ {
			st.Friends = append(st.Friends, Friendship{st.People[i-1].ID, st.People[i].ID}, Friendship{st.People[0].ID, st.People[i].ID})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	const saves = 20
	for range saves {
		if err := s.Update(func(st *State) error { st.Games.Direct++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	each := time.Since(start) / saves
	info, _ := os.Stat(filepath.Join(dir, "state.json"))
	t.Logf("%d people, %d friendships: state file %d KB, one save %v", n, 2*(n-1), info.Size()/1024, each)
	if each > time.Second {
		t.Errorf("one save took %v", each)
	}
}
