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

// Console is one paired console. A removed console keeps its row, so that it can be
// told it was removed rather than ignored like a stranger. LastUsed is when it last
// asked anything, written at most once an hour.
type Console struct {
	Number   int
	Key      []byte
	Paired   time.Time
	Removed  bool
	LastUsed time.Time
}

// Person is one RomM account, on every console it is on. Account is the string a console
// picks for it, the same on each console and across a reinstall. Console is the one used
// last, and Others the rest: a console joins someone with their code, or an invite made
// for them, used by their own account. Console is 0 while they are signed out of every
// console. ChoseOffline is the person's own Offline, which holds on all of them; a
// console's own reasons for Offline are not here. JoinsOnly is an account of an app that
// only joins and never hosts, as 2P Start: it said so at pairing or its last sign-in.
type Person struct {
	ID           string
	Console      int
	Others       []int `json:",omitempty"`
	Account      string
	Username     string
	Note         string
	LastSeen     time.Time
	ChoseOffline bool `json:",omitempty"`
	JoinsOnly    bool `json:",omitempty"`
}

// CanPlay says whether two people could ever play together. One of them has to host, so two
// who only join cannot, and are never made friends.
func CanPlay(a, b *Person) bool {
	return !a.JoinsOnly || !b.JoinsOnly
}

// On says whether a person is on a console.
func (p *Person) On(console int) bool {
	return p.Console == console || slices.Contains(p.Others, console)
}

// Consoles lists a person's consoles, the one used last first.
func (p *Person) Consoles() []int {
	if p.Console == 0 {
		return slices.Clone(p.Others)
	}
	return append([]int{p.Console}, p.Others...)
}

// Use makes a console the one used last, adding it if it is new to them, and says whether
// that changed anything. It makes new slices, never changing the old in place, because a
// snapshot may still be reading them.
func (p *Person) Use(console int) bool {
	if p.Console == console {
		return false
	}
	if p.Console == 0 {
		p.Console = console
		return true
	}
	others := []int{p.Console}
	for _, n := range p.Others {
		if n != console {
			others = append(others, n)
		}
	}
	p.Console, p.Others = console, others
	return true
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
	CodeOwn    = "own"    // the owner's own console: nobody to pick
	CodeInvite = "invite" // a new console, whose account becomes For's friend, or For on another console
	CodeLink   = "link"   // another server, for the account For
	CodePerson = "person" // For's own: each of their consoles, until it is changed. An invite or
	// a console's own code becomes this for whoever used it, so a person has one code from the start.
)

// Code is one code made and not yet typed, or a person's own. Code stays readable until it
// is used, cancelled or expired, because the page shows it again; K is its slow hash, which
// is what a joiner's proof is checked against.
type Code struct {
	ID      string
	Kind    string
	Code    string
	K       []byte
	Note    string
	For     string
	Expires time.Time
}

// Live says whether a code can still be used. A person's code has no expiry: it lasts
// until the owner changes it.
func (c *Code) Live(now time.Time) bool {
	return c.Kind == CodePerson || now.Before(c.Expires)
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

// PersonOn returns the person with an account on a console, or nil.
func (s *State) PersonOn(console int, account string) *Person {
	for i := range s.People {
		if s.People[i].Account == account && s.People[i].On(console) {
			return &s.People[i]
		}
	}
	return nil
}

// PersonFor returns who an account signing in on a console is: the person with that account
// on it, or else on a console in with it, which this console then joins. Nil means someone new.
// A console is in with another when someone is on both, so a console only ever comes in with
// the consoles of the person it joined, never with a stranger's: an account is not a secret.
func (s *State) PersonFor(console int, account string) *Person {
	if p := s.PersonOn(console, account); p != nil {
		return p
	}
	with := map[int]bool{}
	for _, p := range s.People {
		if p.On(console) {
			for _, n := range p.Consoles() {
				with[n] = true
			}
		}
	}
	for i := range s.People {
		if s.People[i].Account != account {
			continue
		}
		for _, n := range s.People[i].Consoles() {
			if with[n] {
				return &s.People[i]
			}
		}
	}
	return nil
}

// CountOn is how many people are on a console.
func (s *State) CountOn(console int) int {
	n := 0
	for i := range s.People {
		if s.People[i].On(console) {
			n++
		}
	}
	return n
}

// SignOut takes a person off every console they are on, and those consoles off the
// server, with whoever else is on them: Change code, and Remove. The others stay on
// those consoles, so typing a code on one brings them back too. It returns the consoles.
func (s *State) SignOut(id string) []int {
	p := s.Person(id)
	if p == nil {
		return nil
	}
	gone := p.Consoles()
	for _, n := range gone {
		if c := s.ConsoleByNumber(n); c != nil {
			c.Removed = true
		}
	}
	p.Console, p.Others = 0, nil
	return gone
}

// RemoveConsole takes a console off everyone on it. Whoever is on no other console is
// removed, with their friendships and pairs; the rest stay, on their other consoles.
func (s *State) RemoveConsole(console int) (gone, stay []string) {
	for i := range s.People {
		p := &s.People[i]
		switch {
		case !p.On(console):
		case len(p.Others) == 0:
			gone = append(gone, p.ID)
		default:
			stay = append(stay, p.ID)
			rest := slices.DeleteFunc(p.Consoles(), func(n int) bool { return n == console })
			p.Console, p.Others = rest[0], rest[1:]
		}
	}
	for _, id := range gone {
		s.RemovePerson(id)
	}
	return gone, stay
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

// CodeOf returns a person's own code, or nil.
func (s *State) CodeOf(person string) *Code {
	for i := range s.Codes {
		if s.Codes[i].Kind == CodePerson && s.Codes[i].For == person {
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

// RemovePerson drops a person, their friendships, their pairs and their code.
func (s *State) RemovePerson(id string) {
	s.People = slices.DeleteFunc(s.People, func(p Person) bool { return p.ID == id })
	s.Friends = slices.DeleteFunc(s.Friends, func(f Friendship) bool { return f.A == id || f.B == id })
	for i := range s.Links {
		s.Links[i].Pairs = slices.DeleteFunc(s.Links[i].Pairs, func(p Pair) bool { return p.Ours == id })
	}
	s.Codes = slices.DeleteFunc(s.Codes, func(c Code) bool { return c.Kind == CodePerson && c.For == id })
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
	for i := range c.People {
		c.People[i].Others = slices.Clone(c.People[i].Others)
	}
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
// how first run knows to ask for one. Only a missing file means none: a file that
// cannot be read, or holds no hash, gives lockedHash, which no password matches, so
// a passing fault or a damaged file cannot hand first run to whoever asks next.
// Deleting the file is still the way back from a forgotten password.
func (s *Store) AdminHash() string {
	var a struct{ Hash string }
	err := s.ReadJSON("admin.json", &a)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil || a.Hash == "":
		return lockedHash
	}
	return a.Hash
}

// lockedHash stands for an admin.json that is there but cannot be used.
const lockedHash = "locked"

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
