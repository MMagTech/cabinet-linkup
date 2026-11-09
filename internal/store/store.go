// Package store keeps the server's data folder: everything that must survive a
// restart, in JSON files written atomically. People, friends, links, codes and
// settings are one file, so copying the folder while the server runs cannot catch
// them mid-change. The admin password is a file of its own, so that deleting it is
// the way back from a forgotten password. The relay's counts are a file of their
// own, because they change every minute while a game is relayed.
//
// Who is online and invites in progress are not here: they stay in memory, and a
// restart clears them.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// What using a link code can come to, for the admin page to say at the right field.
var (
	ErrWrongCode = errors.New("wrong or expired code")
	ErrCantReach = errors.New("can't reach that server")
)

// Format is the state file's format. A server refuses to start on a file from a
// newer one, rather than overwrite what it cannot read. Raise it only when an older
// server would misread the file, never to add a field: going back from :testing to
// :latest on the same folder works only while it stays the same (CONTRIBUTING.md).
const Format = 1

// DefaultStun is the STUN list on a new server, and what Reset brings back.
// Cloudflare first: it answered faster and documents its STUN as free and unlimited.
var DefaultStun = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}

// Limits the admin page holds to, checked here as well as on the page.
const (
	MaxNameLength = 20
	MaxNoteLength = 60
)

// Console is one paired console. A removed console keeps its row, with no people,
// so that it can be told it was removed rather than ignored like a stranger.
type Console struct {
	Number  int
	Key     []byte
	Paired  time.Time
	Removed bool
}

// Person is one RomM account on one console. Account is the string that console
// picked for it, which stays the same across a reinstall.
type Person struct {
	ID       string
	Console  int
	Account  string
	Username string
	Note     string
	LastSeen time.Time
}

// Friendship is two people on this server who can see each other.
type Friendship struct {
	A, B string
}

// Pair is two people across a link who can see each other, and all that a link
// ever says about anybody.
type Pair struct {
	ID          string
	Ours        string
	Theirs      string
	TheirName   string
	TheirKey    []byte
	TheirStream int
	Added       time.Time
}

// Link is another server.
type Link struct {
	ID      string
	Number  int
	Name    string
	Address string
	Key     []byte
	Pairs   []Pair
	Failing time.Time // when it stopped answering, zero while it answers

	// An address this server was told is only a candidate until an Open proves it. Typed says the
	// owner typed Address, and ProvedPrivate that it led to a private address when it was proved:
	// only those may lead to a private address.
	Candidate      string
	CandidateTried time.Time
	Typed          bool
	ProvedPrivate  bool
}

// The kinds of code.
const (
	CodeOwn     = "own"     // the owner's own console: nobody to pick
	CodeInvite  = "invite"  // a new console, whose account becomes For's friend
	CodeConsole = "console" // a reinstall: the same console with a new key
	CodeLink    = "link"    // another server, for the account For
)

// Code is one code made and not yet typed. Code stays readable until it is used,
// cancelled or expired, because the page shows it again; K is its slow hash, which is
// what a joiner's proof is checked against.
type Code struct {
	ID      string
	Kind    string
	Code    string
	K       []byte
	Note    string
	For     string
	Console int
	Person  string // who the page came back to, for a reinstall's code
	Expires time.Time
}

// Stun is one address on the STUN list.
type Stun struct {
	ID      string
	Address string
}

// Settings are what the owner set on the admin page.
type Settings struct {
	Name          string
	Address       string // the public address, free text, only ever handed out
	LocalAddress  string // what the owner's own console types
	Theme         string
	Stun          []Stun
	Relay         bool
	RelayLimitGB  int
	RelayResetDay int
	RelayGames    int // relayed games at once; none yet means the relay's default
}

// Setup is how far first run has got, so that someone who walks away resumes at the
// first unfinished step.
type Setup struct {
	AddressDone bool
	RelayDone   bool
	OwnDone     bool
}

// Games counts how every game on this server connected, since it first started.
type Games struct {
	Direct, Relayed, Failed int
}

// State is the one state file.
type State struct {
	Format      int
	ServerKey   []byte // the server's long-term private key, the most sensitive thing here
	ServerPub   []byte
	RelaySecret []byte
	Settings    Settings
	Setup       Setup
	Consoles    []Console
	People      []Person
	Friends     []Friendship
	Links       []Link
	Codes       []Code
	Games       Games
	NextConsole int
	NextLink    int
}

// Person returns the person with this id, or nil.
func (s *State) Person(id string) *Person {
	for i := range s.People {
		if s.People[i].ID == id {
			return &s.People[i]
		}
	}
	return nil
}

// PersonByAccount returns this console's person for an account, or nil.
func (s *State) PersonByAccount(console int, account string) *Person {
	for i := range s.People {
		if s.People[i].Console == console && s.People[i].Account == account {
			return &s.People[i]
		}
	}
	return nil
}

// ConsoleByNumber returns the console with this number, or nil.
func (s *State) ConsoleByNumber(n int) *Console {
	for i := range s.Consoles {
		if s.Consoles[i].Number == n {
			return &s.Consoles[i]
		}
	}
	return nil
}

// ConsoleByKey returns the console with this public key, or nil.
func (s *State) ConsoleByKey(key []byte) *Console {
	for i := range s.Consoles {
		if slices.Equal(s.Consoles[i].Key, key) {
			return &s.Consoles[i]
		}
	}
	return nil
}

// Link returns the link with this id, or nil.
func (s *State) Link(id string) *Link {
	for i := range s.Links {
		if s.Links[i].ID == id {
			return &s.Links[i]
		}
	}
	return nil
}

// LinkByKey returns the link to the server with this public key, or nil.
func (s *State) LinkByKey(key []byte) *Link {
	for i := range s.Links {
		if slices.Equal(s.Links[i].Key, key) {
			return &s.Links[i]
		}
	}
	return nil
}

// Code returns the code with this id, or nil.
func (s *State) Code(id string) *Code {
	for i := range s.Codes {
		if s.Codes[i].ID == id {
			return &s.Codes[i]
		}
	}
	return nil
}

// RemoveCode drops a code by id.
func (s *State) RemoveCode(id string) {
	s.Codes = slices.DeleteFunc(s.Codes, func(c Code) bool { return c.ID == id })
}

// FriendsOf returns the ids of everyone on this server who is friends with id.
func (s *State) FriendsOf(id string) []string {
	var out []string
	for _, f := range s.Friends {
		switch id {
		case f.A:
			out = append(out, f.B)
		case f.B:
			out = append(out, f.A)
		}
	}
	return out
}

// AreFriends says whether two people on this server are friends.
func (s *State) AreFriends(a, b string) bool {
	for _, f := range s.Friends {
		if (f.A == a && f.B == b) || (f.A == b && f.B == a) {
			return true
		}
	}
	return false
}

// AddFriends makes two people friends, if they are not already.
func (s *State) AddFriends(a, b string) {
	if a != b && !s.AreFriends(a, b) {
		s.Friends = append(s.Friends, Friendship{A: a, B: b})
	}
}

// RemoveFriends ends a friendship.
func (s *State) RemoveFriends(a, b string) {
	s.Friends = slices.DeleteFunc(s.Friends, func(f Friendship) bool {
		return (f.A == a && f.B == b) || (f.A == b && f.B == a)
	})
}

// RemovePerson drops a person, their friendships and their pairs.
func (s *State) RemovePerson(id string) {
	s.People = slices.DeleteFunc(s.People, func(p Person) bool { return p.ID == id })
	s.Friends = slices.DeleteFunc(s.Friends, func(f Friendship) bool { return f.A == id || f.B == id })
	for i := range s.Links {
		s.Links[i].Pairs = slices.DeleteFunc(s.Links[i].Pairs, func(p Pair) bool { return p.Ours == id })
	}
	for i := range s.Codes {
		if s.Codes[i].For == id {
			s.Codes[i].For = ""
		}
	}
}

// NewID makes an id for a person, a code, a link, a pair or a STUN line.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Store is the data folder, and the one lock over the state.
type Store struct {
	dir   string
	mu    sync.RWMutex
	state State
	dirty bool
}

// Open reads the data folder, or makes a new one, calling fresh to fill in the keys
// and defaults of a server that has never started.
func Open(dir string, fresh func(*State) error) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("the data folder cannot be made: %w", err)
	}
	s := &Store{dir: dir}
	err := s.ReadJSON("state.json", &s.state)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = State{Format: Format, NextConsole: 1, NextLink: 1}
		s.state.Settings.RelayLimitGB = 500
		s.state.Settings.RelayResetDay = 1
		for _, a := range DefaultStun {
			s.state.Settings.Stun = append(s.state.Settings.Stun, Stun{ID: NewID(), Address: a})
		}
		if err := fresh(&s.state); err != nil {
			return nil, err
		}
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("the data folder cannot be written: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("the state file cannot be read: %w", err)
	case s.state.Format > Format:
		return nil, fmt.Errorf("the state file is format %d and this server knows only %d: update the server, or restore an older backup", s.state.Format, Format)
	}
	return s, nil
}

// View reads the state under the lock. What it reads must not outlive the call.
func (s *Store) View(read func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	read(&s.state)
}

// Update changes the state and saves it before returning. If change returns an error
// nothing is saved, so change must not have altered anything by then.
func (s *Store) Update(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := change(&s.state); err != nil {
		return err
	}
	return s.save()
}

// Change is Update for a change that may turn out to be nothing: it saves only if change says it
// changed something, so a quiet link does not wear out an SD card.
func (s *Store) Change(change func(*State) (changed bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !change(&s.state) {
		return nil
	}
	return s.save()
}

// Snapshot copies the state, for reading without holding the lock.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.state
	c.Consoles = slices.Clone(s.state.Consoles)
	c.People = slices.Clone(s.state.People)
	c.Friends = slices.Clone(s.state.Friends)
	c.Codes = slices.Clone(s.state.Codes)
	c.Settings.Stun = slices.Clone(s.state.Settings.Stun)
	c.Links = slices.Clone(s.state.Links)
	for i := range c.Links {
		c.Links[i].Pairs = slices.Clone(c.Links[i].Pairs)
	}
	return c
}

// Touch changes the state and saves it at the next Flush: for what changes often and
// matters little, such as when someone was last seen.
func (s *Store) Touch(change func(*State) (changed bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if change(&s.state) {
		s.dirty = true
	}
}

// Flush saves what Touch left, if anything.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.save()
}

func (s *Store) save() error {
	s.state.Format = Format
	if err := s.WriteJSON("state.json", &s.state); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// AdminHash returns the admin password's hash, or "" if none has been set, which is
// how first run knows to ask for one.
func (s *Store) AdminHash() string {
	var a struct{ Hash string }
	if err := s.ReadJSON("admin.json", &a); err != nil {
		return ""
	}
	return a.Hash
}

// SetAdminHash saves the admin password's hash.
func (s *Store) SetAdminHash(hash string) error {
	return s.WriteJSON("admin.json", struct{ Hash string }{hash})
}

// ReadJSON reads a file in the data folder into v.
func (s *Store) ReadJSON(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteJSON replaces a file in the data folder with v, whole or not at all.
func (s *Store) WriteJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return WriteFile(filepath.Join(s.dir, name), append(data, '\n'))
}

// Dir is the data folder.
func (s *Store) Dir() string {
	return s.dir
}

// WriteFile replaces a file whole or not at all: to a new file, flushed to the disk,
// then renamed over the old one. Only the server's own user can read it.
func WriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
