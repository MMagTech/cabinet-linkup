package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/standin"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	gameName = "Contra III The Alien Wars"
	secret   = "correct horse"
)

func udpPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// box is one real server and a browser signed in to its admin page.
type box struct {
	t       *testing.T
	s       *Server
	dir     string
	address string
	admin   http.Client
	cookie  []*http.Cookie
}

func startServer(t *testing.T, name string) *box {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{
		Data: dir, ConsoleAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0",
		RelayPort: udpPort(t), RelayPrivate: true, Version: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(false)
	t.Cleanup(func() { s.Stop() })
	b := &box{t: t, s: s, dir: dir, address: s.ConsoleAddress()}
	b.admin.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b.form("/setup/password", "password", secret, "again", secret)
	b.form("/setup/address", "name", name, "address", b.address)
	b.get("/setup/relay/skip")
	b.get("/setup/own/skip")
	return b
}

func (b *box) do(req *http.Request) string {
	b.t.Helper()
	for _, c := range b.cookie {
		req.AddCookie(c)
	}
	resp, err := b.admin.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		b.cookie = cs
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body) + "\nLOCATION " + resp.Header.Get("Location")
}

func (b *box) get(path string) string {
	b.t.Helper()
	req, _ := http.NewRequest("GET", "http://"+b.s.AdminAddress()+path, nil)
	return b.do(req)
}

func (b *box) form(path string, kv ...string) string {
	b.t.Helper()
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	req, _ := http.NewRequest("POST", "http://"+b.s.AdminAddress()+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

// code makes a code on the admin page, through the real forms, and returns it.
func (b *box) code(path, query string, kv ...string) string {
	b.t.Helper()
	b.form(path, kv...)
	st := b.s.Store.Snapshot()
	return st.Codes[len(st.Codes)-1].Code
}

func (b *box) ownCode() string {
	return b.code("/setup/own", "", "address", b.address)
}

func (b *box) invite(forName string) string {
	st := b.s.Store.Snapshot()
	id := ""
	for _, p := range st.People {
		if p.Username == forName {
			id = p.ID
		}
	}
	return b.code("/people/add", "", "note", "for "+forName, "for", id)
}

func (b *box) person(name string) string {
	st := b.s.Store.Snapshot()
	for _, p := range st.People {
		if p.Username == name {
			return p.ID
		}
	}
	b.t.Fatalf("nobody called %s", name)
	return ""
}

func (b *box) linkCode(forName string) string {
	return b.code("/links/make", "", "for", b.person(forName))
}

// player is a stand-in console with one account signed in and Online.
func player(t *testing.T, address, code, name string) *standin.Console {
	t.Helper()
	c, err := standin.New(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(code, "acct-"+name, name); err != nil {
		t.Fatalf("%s could not pair: %v", name, err)
	}
	if _, err := c.Signin("acct-"+name, name, "online"); err != nil {
		t.Fatal(err)
	}
	return c
}

func friendsOf(t *testing.T, c *standin.Console) map[string]standin.Friend {
	t.Helper()
	list, err := c.Friends()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]standin.Friend{}
	for _, f := range list {
		out[f.Username] = f
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("it never happened: %s", what)
}

func nextEvent(t *testing.T, c *standin.Console, kind string) standin.Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		events, err := c.Wait()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type == kind {
				return e
			}
		}
	}
	t.Fatalf("no %s event", kind)
	return standin.Event{}
}

func TestPairingAndAFullGameOnOneServer(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")

	friends := friendsOf(t, alex)
	if friends["sam"].Status != "online" || friends["sam"].Server != "Ours" {
		t.Fatalf("%+v", friends)
	}

	id := standin.NewInviteID()
	box, err := alex.Seal(friends["sam"].Key, id, []byte(gameName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alex.Invite(id, friends["sam"].ID, box); err != nil {
		t.Fatal(err)
	}
	e := nextEvent(t, sam, "invite")
	if e.Invite != id || e.Sealed != box {
		t.Fatalf("%+v", e)
	}
	opened, err := sam.Open(friendsOf(t, sam)["alex"].Key, id, e.Sealed)
	if err != nil || string(opened) != gameName {
		t.Fatalf("%q %v", opened, err)
	}
	if friendsOf(t, sam)["alex"].Status != "linkedup" {
		t.Error("the inviter is Linked up")
	}
	if _, err := sam.Join(id); err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, alex, "joined"); got.Session != id {
		t.Fatalf("%+v", got)
	}
	if err := alex.Signal(id, "offer"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, sam, "signal"); got.Sealed != "offer" {
		t.Fatalf("%+v", got)
	}
	if err := alex.Report(id, "direct", "none"); err != nil {
		t.Fatal(err)
	}
	if err := sam.End(id); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, alex, "session_ended")
	if st := b.s.Store.Snapshot(); st.Games.Direct != 1 {
		t.Errorf("%+v", st.Games)
	}
}

func TestWrongAndUsedAndExpiredCodes(t *testing.T) {
	b := startServer(t, "Ours")
	code := b.ownCode()
	c, _ := standin.New(b.address)
	if err := c.Pair("K7MR-4QXT-9HPV", "a", "a"); !errors.Is(err, proof.ErrWrongCode) {
		t.Errorf("a wrong code: %v", err)
	}
	player(t, b.address, code, "first")
	c2, _ := standin.New(b.address)
	if err := c2.Pair(code, "a", "a"); !errors.Is(err, proof.ErrWrongCode) {
		t.Errorf("a code works once: %v", err)
	}
	code = b.invite("first")
	b.s.Store.Update(func(st *store.State) error {
		st.Codes[len(st.Codes)-1].Expires = time.Now().Add(-time.Second)
		return nil
	})
	c3, _ := standin.New(b.address)
	if err := c3.Pair(code, "a", "a"); !errors.Is(err, proof.ErrWrongCode) {
		t.Errorf("an expired code: %v", err)
	}
}

func TestCantReachAServer(t *testing.T) {
	c, _ := standin.New("127.0.0.1:1")
	if err := c.Pair("K7MR-4QXT-9HPV", "a", "a"); !errors.Is(err, proof.ErrUnreachable) {
		t.Errorf("%v", err)
	}
}

func TestAStrangersGuessesCannotLockARealCodeOut(t *testing.T) {
	b := startServer(t, "Ours")
	code := b.ownCode()
	stranger, _ := standin.New(b.address)
	sawLimit := false
	for range 40 {
		if err := stranger.Pair("K7MR-4QXT-9HPV", "a", "a"); errors.Is(err, proof.ErrTooManyTries) {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Error("guesses should reach the limit")
	}
	player(t, b.address, code, "real")
}

func TestAnythingElseGetsNothing(t *testing.T) {
	b := startServer(t, "Ours")
	base := "http://" + b.address
	get := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/", ""},
		{"GET", "/status", ""},
		{"GET", "/linkup/v1", ""},
		{"POST", "/", "x"},
		{"POST", "/linkup/v2", "x"},
		{"POST", "/linkup/v1", ""},
		{"POST", "/linkup/v1", "\x09junk"},
		{"POST", "/linkup/v1", "\x01short"},
		{"POST", "/linkup/v1", "\x03" + strings.Repeat("x", 96)},
		{"POST", "/linkup/v1", "\x04" + strings.Repeat("x", 40)},
		{"POST", "/linkup/v1", strings.Repeat("x", proof.MaxBody+10)},
	} {
		status, body := get(c.method, c.path, c.body)
		if status != http.StatusNotFound || body != "" {
			t.Errorf("%s %s: %d %q", c.method, c.path, status, body)
		}
	}
}

func TestOpenFromAStrangersKeyGetsNothing(t *testing.T) {
	b := startServer(t, "Ours")
	stranger, _ := proof.NewKeyPair()
	c := proof.NewClient(b.address, stranger, b.s.Key.Public)
	if err := c.Prove(t.Context()); err == nil {
		t.Error("an unknown key must get nothing")
	}
}

func TestAFakeServerCannotPass(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	// Another real server at the address the console was told: it has no key the console trusts.
	fake := startServer(t, "Fake")
	alex.Client.SetAddress(fake.address)
	if err := alex.Status("online"); err == nil {
		t.Error("the console must refuse a server without the remembered key")
	}
	alex.Client.SetAddress(b.address)
	if err := alex.Status("online"); err != nil {
		t.Errorf("back at the real one: %v", err)
	}
}

// A fake server during pairing: it does not hold the code's slow hash, so it cannot answer message 1.
func TestAFakeServerCannotPassDuringPairing(t *testing.T) {
	fake := startServer(t, "Fake")
	fake.ownCode()
	c, _ := standin.New(fake.address)
	if err := c.Pair("K7MR-4QXT-9HPV", "a", "a"); err == nil {
		t.Error("it holds a different code, so the console's must fail")
	}
}

func TestRetypingAnAddressNeedsNoNewCode(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	host, port, _ := net.SplitHostPort(b.address)
	for _, address := range []string{"http://" + b.address, "localhost:" + port, host + ":" + port, "http://" + b.address + "/"} {
		alex.Client.SetAddress(address)
		alex.Client.Prove(t.Context())
		if err := alex.Status("online"); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
}

func TestBehindAReverseProxy(t *testing.T) {
	b := startServer(t, "Ours")
	target, _ := url.Parse("http://" + b.address)
	proxy := httptest.NewServer(http.StripPrefix("/games", httputil.NewSingleHostReverseProxy(target)))
	defer proxy.Close()
	alex := player(t, proxy.URL+"/games", b.ownCode(), "alex")
	if _, err := alex.Friends(); err != nil {
		t.Fatal(err)
	}
	proxied := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxied.Close()
	sam := player(t, proxied.URL, b.invite("alex"), "sam")
	if len(friendsOf(t, sam)) != 1 {
		t.Error("a self-signed proxy must work")
	}
}

// recorder is someone in the middle: it sees everything and can change or repeat it.
type recorder struct {
	mu       sync.Mutex
	seen     bytes.Buffer
	requests [][]byte
	tamper   func(body []byte) []byte
	target   string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	body, _ := io.ReadAll(q.Body)
	r.mu.Lock()
	r.seen.Write(body)
	r.requests = append(r.requests, bytes.Clone(body))
	tamper := r.tamper
	r.mu.Unlock()
	if tamper != nil {
		body = tamper(body)
	}
	req, _ := http.NewRequest(q.Method, "http://"+r.target+q.URL.Path, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	r.mu.Lock()
	r.seen.Write(out)
	r.mu.Unlock()
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

func throughRecorder(t *testing.T, target string) (*recorder, string) {
	r := &recorder{target: target}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return r, srv.URL
}

// The traffic search: everything on the console port during a full run, searched for what must
// never appear.
func TestTrafficHoldsNothingReadable(t *testing.T) {
	b := startServer(t, "Ours")
	r, address := throughRecorder(t, b.address)
	ownCode := b.ownCode()
	alex := player(t, address, ownCode, "alex-the-player")
	inviteCode := b.invite("alex-the-player")
	sam := player(t, address, inviteCode, "sam-the-friend")

	id := standin.NewInviteID()
	friends := friendsOf(t, alex)
	box, _ := alex.Seal(friends["sam-the-friend"].Key, id, []byte(gameName))
	alex.Invite(id, friends["sam-the-friend"].ID, box)
	sam.Join(id)
	sam.Friends()
	alex.Signal(id, box)
	nextEvent(t, sam, "signal")
	alex.Report(id, "direct", "none")
	alex.End(id)

	seen := r.seen.Bytes()
	tidy1, _ := proof.TidyCode(ownCode)
	tidy2, _ := proof.TidyCode(inviteCode)
	for _, word := range []string{ownCode, inviteCode, tidy1, tidy2, "alex-the-player", "sam-the-friend", "acct-", gameName, "Contra", "Ours", "invite", "friends", "online", "\"op\"", "stun"} {
		if bytes.Contains(seen, []byte(word)) {
			t.Errorf("%q is readable on the wire", word)
		}
	}
	for _, key := range [][]byte{b.s.Key.Private, alex.Key.Private, sam.Key.Private} {
		if bytes.Contains(seen, key) {
			t.Error("a private key is on the wire")
		}
	}
	if len(seen) < 1000 {
		t.Errorf("that recording is suspiciously small: %d", len(seen))
	}
}

func TestChangingAnyByteIsRefused(t *testing.T) {
	b := startServer(t, "Ours")
	r, address := throughRecorder(t, b.address)
	alex := player(t, address, b.ownCode(), "alex")
	r.mu.Lock()
	r.tamper = func(body []byte) []byte {
		body = bytes.Clone(body)
		body[len(body)-1] ^= 1
		return body
	}
	r.mu.Unlock()
	if err := alex.Status("online"); err == nil {
		t.Error("a changed byte must fail")
	}
}

func TestReplayedRequestsAreRefused(t *testing.T) {
	b := startServer(t, "Ours")
	r, address := throughRecorder(t, b.address)
	alex := player(t, address, b.ownCode(), "alex")
	alex.Status("offline")
	alex.Status("online")
	r.mu.Lock()
	recorded := r.requests
	r.mu.Unlock()
	sealedSeen, opensSeen := 0, 0
	for _, body := range recorded {
		switch body[0] {
		case proof.KindSealed:
			sealedSeen++
		case proof.KindOpen:
			opensSeen++
		default:
			continue
		}
		resp, err := http.Post("http://"+b.address+proof.Path, "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("a replay of kind %d got %d", body[0], resp.StatusCode)
		}
	}
	if sealedSeen == 0 || opensSeen == 0 {
		t.Errorf("recorded %d sealed, %d opens", sealedSeen, opensSeen)
	}
	if err := alex.Status("online"); err != nil {
		t.Errorf("replays must not push a console off: %v", err)
	}
}

// One recorded Open, replayed as fast as it can go, cannot keep a console from coming back.
func TestAReplayedOpenCannotLockAConsoleOut(t *testing.T) {
	b := startServer(t, "Ours")
	r, address := throughRecorder(t, b.address)
	alex := player(t, address, b.ownCode(), "alex")
	var open []byte
	r.mu.Lock()
	for _, body := range r.requests {
		if body[0] == proof.KindOpen {
			open = body
		}
	}
	r.mu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Post("http://"+b.address+proof.Path, "application/octet-stream", bytes.NewReader(open))
			if err == nil {
				resp.Body.Close()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for range 20 {
		if err := alex.Client.Prove(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := alex.Status("online"); err != nil {
			t.Fatalf("locked out: %v", err)
		}
	}
}

func TestALostPairingReplyCanBeAskedForAgain(t *testing.T) {
	b := startServer(t, "Ours")
	r, address := throughRecorder(t, b.address)
	code := b.ownCode()
	player(t, address, code, "alex")
	var third []byte
	r.mu.Lock()
	for _, body := range r.requests {
		if body[0] == proof.KindPairSecond {
			third = body
		}
	}
	r.mu.Unlock()
	post := func() (int, []byte) {
		resp, err := http.Post("http://"+b.address+proof.Path, "application/octet-stream", bytes.NewReader(third))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	s1, a := post()
	s2, c := post()
	if s1 != 200 || s2 != 200 || !bytes.Equal(a, c) {
		t.Errorf("%d %d: an identical message 3 gets identical bytes", s1, s2)
	}
	if st := b.s.Store.Snapshot(); len(st.Consoles) != 1 {
		t.Errorf("it must not pair twice: %+v", st.Consoles)
	}
}

func TestAnInviteCodeCannotBecomeALink(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	_ = alex
	code := b.invite("alex")
	key, _ := proof.NewKeyPair()
	c := proof.NewClient(b.address, key, nil)
	tidy, _ := proof.TidyCode(code)
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_, err := c.Pair(t.Context(), tidy, map[string]any{"kind": "server", "name": "Evil", "address": "192.168.1.1:80", "account": map[string]any{"id": "x", "username": "x"}}, &reply)
	if err == nil && reply.OK {
		t.Fatal("a friend's invite became a linked server")
	}
	st := b.s.Store.Snapshot()
	if len(st.Links) != 0 {
		t.Errorf("%+v", st.Links)
	}
	if st.Code(st.Codes[0].ID) == nil || st.Codes[0].Code != code {
		t.Error("a refused kind must not use the code")
	}
	player(t, b.address, code, "sam")
}

func TestALinkCodeCannotBecomeAConsole(t *testing.T) {
	b := startServer(t, "Ours")
	player(t, b.address, b.ownCode(), "alex")
	code := b.linkCode("alex")
	c, _ := standin.New(b.address)
	if err := c.Pair(code, "a", "a"); err == nil {
		t.Fatal("a link code made a console")
	}
	if st := b.s.Store.Snapshot(); len(st.Consoles) != 1 || len(st.Codes) != 1 {
		t.Errorf("%+v", st)
	}
}

func TestAConsoleCannotSpeakAsAServer(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	var raw struct {
		Error string `json:"error"`
	}
	for _, op := range []string{"link_hello", "link_unlink", "link_invite", "link_relay"} {
		if err := alex.Client.Call(t.Context(), map[string]any{"op": op}, &raw); err != nil {
			t.Fatal(err)
		}
		if raw.Error != "unknown" {
			t.Errorf("%s: %q", op, raw.Error)
		}
		raw.Error = ""
	}
}

func TestAPairedKeyCannotTakeASecondPlace(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	code := b.invite("alex")
	again := &standin.Console{Key: alex.Key, Client: proof.NewClient(b.address, alex.Key, nil)}
	if err := again.Pair(code, "acct-alex", "alex"); err == nil {
		t.Error("a key already here takes friends from the owner's page, not a code")
	}
	if st := b.s.Store.Snapshot(); len(st.Codes) != 1 {
		t.Error("and the code is not used")
	}
}

func TestReinstallKeepsAccountsAndFriends(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")
	number := alex.Number
	person := b.person("alex")

	code := b.code("/consoles/new-code", "", "id", fmt.Sprint(number), "person", person)
	reinstalled := player(t, b.address, code, "alex")
	if reinstalled.Number != number {
		t.Errorf("console %d became %d", number, reinstalled.Number)
	}
	if b.person("alex") != person {
		t.Error("the account must keep its person")
	}
	if friendsOf(t, reinstalled)["sam"].Username != "sam" {
		t.Error("it gets its friends back when it signs in")
	}
	if !bytes.Equal(friendsOf(t, sam)["alex"].Key, reinstalled.Key.Public) {
		t.Error("the friend hears the new key")
	}
	if err := alex.Status("online"); err == nil {
		t.Error("the old key must be finished")
	}
}

func TestTheWrongAccountSignedIn(t *testing.T) {
	b := startServer(t, "Ours")
	player(t, b.address, b.ownCode(), "owner")
	code := b.invite("owner")
	c := player(t, b.address, code, "kid")
	if _, ok := friendsOf(t, c)["owner"]; !ok {
		t.Fatal("the account signed in when the code was typed becomes the friend")
	}
	// The fix on People, with no new code.
	b.form("/friends/remove", "person", b.person("kid"), "id", "f:"+b.person("owner"))
	if _, err := c.Signin("acct-parent", "parent", "online"); err != nil {
		t.Fatal(err)
	}
	b.form("/friends/add", "person", b.person("parent"), "friend", b.person("owner"))
	if _, ok := friendsOf(t, c)["owner"]; !ok {
		t.Error("the right account has the friend")
	}
	c.Signin("acct-kid", "kid", "online")
	if len(friendsOf(t, c)) != 0 {
		t.Error("the wrong one has none")
	}
}

func TestRemovedConsoleIsTold(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")
	b.form("/consoles/remove", "id", fmt.Sprint(sam.Number))
	if len(friendsOf(t, alex)) != 0 {
		t.Error("they leave the list, with no notice")
	}
	nextEvent(t, sam, "removed")
	if err := sam.Status("online"); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("%v", err)
	}
	fresh := &standin.Console{Key: sam.Key, Client: proof.NewClient(b.address, sam.Key, b.s.Key.Public)}
	if err := fresh.Status("online"); !errors.Is(err, proof.ErrRemoved) {
		t.Errorf("a new session must hear it, proved: %v", err)
	}
}

func TestLeaving(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")
	if err := sam.Leave(); err != nil {
		t.Fatal(err)
	}
	if len(friendsOf(t, alex)) != 0 {
		t.Error("gone")
	}
	st := b.s.Store.Snapshot()
	if len(st.People) != 1 || len(st.Consoles) != 1 {
		t.Errorf("%+v", st.People)
	}
}

func TestStunListComesFromTheServer(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	name, stun, err := alex.Config()
	if err != nil || name != "Ours" || len(stun) != 2 || stun[0] != store.DefaultStun[0] {
		t.Errorf("%q %v %v", name, stun, err)
	}
	b.form("/stun/add", "address", "stun.example.net:3478")
	_, stun, _ = alex.Config()
	if len(stun) != 3 {
		t.Errorf("%v", stun)
	}
}

func TestDataSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Data: dir, ConsoleAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", RelayPort: udpPort(t), Version: "1"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Start(false)
	key := s.Key
	s.Store.Update(func(st *store.State) error { st.Settings.Name = "Kept"; return nil })
	s.Stop()
	again, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Console.DropLink("x")
	again.Start(false)
	defer again.Stop()
	if !bytes.Equal(again.Key.Public, key.Public) {
		t.Error("the server's key must stay")
	}
	st := again.Store.Snapshot()
	if st.Settings.Name != "Kept" {
		t.Error(st.Settings.Name)
	}
}

// ---- Two servers ----

func linkUp(t *testing.T, a, c *box, aName, cName string) {
	t.Helper()
	code := a.linkCode(aName)
	body := c.form("/links/enter", "address", a.address, "code", code, "for", c.person(cName))
	if !strings.Contains(body, "LOCATION /links") {
		t.Fatalf("linking failed: %s", body[len(body)-200:])
	}
}

// twoLinked is two servers, each with its own console, with one pair across them.
func twoLinked(t *testing.T) (a, c *box, maya, june *standin.Console) {
	t.Helper()
	a, c = startServer(t, "Alex's"), startServer(t, "Riley's")
	maya = player(t, a.address, a.ownCode(), "maya")
	june = player(t, c.address, c.ownCode(), "june")
	linkUp(t, a, c, "maya", "june")
	eventually(t, "june shows online on the other server", func() bool {
		return friendsOf(t, maya)["june"].Status == "online"
	})
	eventually(t, "maya shows online on the other server", func() bool {
		return friendsOf(t, june)["maya"].Status == "online"
	})
	return a, c, maya, june
}

func TestLinkingPairsOnlyTheAccountsPicked(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	if f := friendsOf(t, maya)["june"]; f.Server != "Riley's" || !bytes.Equal(f.Key, june.Key.Public) {
		t.Errorf("%+v", f)
	}
	other := player(t, a.address, a.invite("maya"), "other")
	if _, ok := friendsOf(t, other)["june"]; ok {
		t.Error("someone else on that server must not see them")
	}
	third := player(t, c.address, c.invite("june"), "third")
	if _, ok := friendsOf(t, third)["maya"]; ok {
		t.Error("nor on the other")
	}
	if _, ok := friendsOf(t, maya)["third"]; ok {
		t.Error("nor can they see anyone else's")
	}
	if st := a.s.Store.Snapshot(); len(st.Links) != 1 || len(st.Links[0].Pairs) != 1 {
		t.Errorf("%+v", st.Links)
	}
}

func TestMorePairsOnOneLink(t *testing.T) {
	a, c, maya, _ := twoLinked(t)
	kid := player(t, a.address, a.invite("maya"), "kid")
	theo := player(t, c.address, c.invite("june"), "theo")
	linkUp(t, a, c, "kid", "theo")
	if st := a.s.Store.Snapshot(); len(st.Links) != 1 || len(st.Links[0].Pairs) != 2 {
		t.Errorf("one link, two pairs: %+v", st.Links)
	}
	eventually(t, "theo", func() bool { return friendsOf(t, kid)["theo"].Status == "online" })
	if _, ok := friendsOf(t, maya)["theo"]; ok {
		t.Error("maya has only june")
	}
	_ = theo
}

func TestNoChainToAThird(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	d := startServer(t, "Third")
	kit := player(t, d.address, d.ownCode(), "kit")
	linkUp(t, c, d, "june", "kit")
	eventually(t, "june sees kit", func() bool { return friendsOf(t, june)["kit"].Status == "online" })
	if _, ok := friendsOf(t, maya)["kit"]; ok {
		t.Error("two owners both linked to a third do not see each other")
	}
	if _, ok := friendsOf(t, kit)["maya"]; ok {
		t.Error("nor the other way")
	}
	if st := a.s.Store.Snapshot(); len(st.Links) != 1 {
		t.Errorf("%+v", st.Links)
	}
}

func TestAGameAcrossALink(t *testing.T) {
	_, _, maya, june := twoLinked(t)
	id := standin.NewInviteID()
	f := friendsOf(t, maya)["june"]
	box, _ := maya.Seal(f.Key, id, []byte(gameName))
	if _, err := maya.Invite(id, f.ID, box); err != nil {
		t.Fatal(err)
	}
	e := nextEvent(t, june, "invite")
	opened, err := june.Open(friendsOf(t, june)["maya"].Key, id, e.Sealed)
	if err != nil || string(opened) != gameName {
		t.Fatalf("%q %v", opened, err)
	}
	if _, err := june.Join(id); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, maya, "joined")
	if err := maya.Signal(id, "offer"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, june, "signal"); got.Sealed != "offer" {
		t.Errorf("%+v", got)
	}
	if err := june.Signal(id, "answer"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, maya, "signal"); got.Sealed != "answer" {
		t.Errorf("%+v", got)
	}
	if err := june.End(id); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, maya, "session_ended")
}

func TestDeclineAndCancelAcrossALink(t *testing.T) {
	_, _, maya, june := twoLinked(t)
	f := friendsOf(t, maya)["june"]
	id, _ := maya.Invite(standin.NewInviteID(), f.ID, "sealed")
	nextEvent(t, june, "invite")
	june.Decline(id)
	if e := nextEvent(t, maya, "invite_ended"); e.Reason != "declined" {
		t.Errorf("%+v", e)
	}
	eventually(t, "maya is free", func() bool { return friendsOf(t, june)["maya"].Status == "online" })
	id, err := maya.Invite(standin.NewInviteID(), f.ID, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	nextEvent(t, june, "invite")
	maya.Cancel(id)
	if e := nextEvent(t, june, "invite_ended"); e.Reason != "cancelled" {
		t.Errorf("%+v", e)
	}
	if _, err := june.Join(id); err == nil {
		t.Error("cancelled")
	}
}

func TestAKnockOnSomeoneOfflineAcrossALink(t *testing.T) {
	_, _, maya, june := twoLinked(t)
	june.Status("offline")
	eventually(t, "june shows offline", func() bool { return friendsOf(t, maya)["june"].Status == "offline" })
	if _, err := maya.Invite(standin.NewInviteID(), friendsOf(t, maya)["june"].ID, "sealed"); err == nil {
		t.Error("a knock on someone Offline is refused")
	}
}

func TestRemovingAPairAndBringingThemBack(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	st := a.s.Store.Snapshot()
	a.form("/links/pairs/remove", "pair", st.Links[0].Pairs[0].ID)
	if len(friendsOf(t, maya)) != 0 {
		t.Error("gone here")
	}
	eventually(t, "gone there", func() bool { return len(friendsOf(t, june)) == 0 })
	linkUp(t, a, c, "maya", "june")
	eventually(t, "back", func() bool { return friendsOf(t, maya)["june"].Status == "online" })
}

func TestUnlinking(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	st := a.s.Store.Snapshot()
	a.form("/links/remove", "id", st.Links[0].ID)
	if len(friendsOf(t, maya)) != 0 {
		t.Error("gone here")
	}
	eventually(t, "gone there", func() bool { return len(friendsOf(t, june)) == 0 })
	eventually(t, "no link", func() bool { return len(c.s.Store.Snapshot().Links) == 0 })
}

func TestOneServerGoingAway(t *testing.T) {
	_, c, maya, june := twoLinked(t)
	_ = june
	c.s.Stop()
	if err := maya.Status("online"); err != nil {
		t.Errorf("the other keeps playing with their own people: %v", err)
	}
}

// A linked server's told address is only a candidate: it can name something on your network, and
// nothing there can finish the handshake.
func TestAToldAddressMustProveItself(t *testing.T) {
	a, c, _, _ := twoLinked(t)
	hits := 0
	var mu sync.Mutex
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer inside.Close()
	aState := a.s.Store.Snapshot()
	link := aState.Links[0]
	before := link.Address
	if link.Address == "" {
		t.Fatal("the typed side should already have an address")
	}
	// c tells a about an address that leads to something on a's network.
	raw, _ := json.Marshal(map[string]any{"op": "link_hello", "name": "Riley's", "address": strings.TrimPrefix(inside.URL, "http://"), "pairs": []any{}})
	a.s.Links.Handle(link.ID, "link_hello", raw)
	a.s.Links.Changed()
	eventually(t, "it was tried", func() bool {
		st := a.s.Store.Snapshot()
		return st.Links[0].Candidate != "" && !st.Links[0].CandidateTried.IsZero()
	})
	st := a.s.Store.Snapshot()
	if st.Links[0].Address != before {
		t.Errorf("the address changed to %q without proof", st.Links[0].Address)
	}
	_ = c
	for _, bad := range []string{
		"http://192.168.1.1/action?x=", "192.168.1.1/a#b", "user@192.168.1.1", "ftp://192.168.1.1",
		"192.168.1.1 /x", "a\nb", "",
	} {
		if proof.PushedAddressOK(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	hit := false
	inside := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer inside.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside.URL+"/router/reboot", http.StatusFound)
	}))
	defer redirector.Close()
	key, _ := proof.NewKeyPair()
	peer, _ := proof.NewKeyPair()
	c := proof.NewClient(redirector.URL, key, peer.Public)
	c.Prove(t.Context())
	if hit {
		t.Error("a redirect was followed")
	}
}

// A told address is tried at most once per 10 minutes, however many a linked owner sends.
func TestToldAddressesAreTriedAtTheirOwnPace(t *testing.T) {
	a, _, _, _ := twoLinked(t)
	var mu sync.Mutex
	hits := 0
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer inside.Close()
	id := a.s.Store.Snapshot().Links[0].ID
	a.s.Store.Update(func(st *store.State) error {
		st.Links[0].CandidateTried = time.Now().Add(-time.Hour)
		return nil
	})
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(inside.URL, "http://"))
	for i := range 15 {
		raw, _ := json.Marshal(map[string]any{"op": "link_hello", "name": "Riley's", "address": fmt.Sprintf("http://%s:%s/x%d", host, port, i), "pairs": []any{}})
		a.s.Links.Handle(id, "link_hello", raw)
		a.s.Links.Changed()
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if hits > 1 {
		t.Errorf("%d requests inside the network", hits)
	}
}

func TestTwoRequestsAtOnceOnANewClientBothWork(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	for round := range 40 {
		c := proof.NewClient(b.address, alex.Key, b.s.Key.Public)
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var reply map[string]any
				errs <- c.Call(t.Context(), map[string]any{"op": "config"}, &reply)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}

func TestAFriendsNewKeyReachesTheirFriendsConsole(t *testing.T) {
	a, _, maya, _ := twoLinked(t)
	id := a.s.Store.Snapshot().Links[0].ID
	pair := a.s.Store.Snapshot().Links[0].Pairs[0]
	newKey, _ := proof.NewKeyPair()
	raw, _ := json.Marshal(map[string]any{"op": "link_hello", "name": "Riley's", "address": a.s.Store.Snapshot().Links[0].Address, "pairs": []any{
		map[string]any{"ours": pair.Theirs, "theirs": pair.Ours, "status": "online", "username": "june", "key": newKey.Public, "stream": 1},
	}})
	a.s.Links.Handle(id, "link_hello", raw)
	nextEvent(t, maya, "friends")
	if !bytes.Equal(friendsOf(t, maya)["june"].Key, newKey.Public) {
		t.Error("the console should hear the new key")
	}
}

func TestAChangedAddressReachesALinkedServerOnItsOwn(t *testing.T) {
	a, c, _, _ := twoLinked(t)
	// c is the side that typed a's address, a was told c's. a can reach c through its proved one.
	eventually(t, "the told address proves itself", func() bool {
		return a.s.Store.Snapshot().Links[0].Address == c.address
	})
	st := a.s.Store.Snapshot()
	if st.Links[0].Typed {
		t.Error("it was told, not typed")
	}
	cst := c.s.Store.Snapshot()
	if cst.Links[0].Address != a.address || !cst.Links[0].Typed {
		t.Errorf("%+v", cst.Links[0])
	}
}

func TestLogsHoldNothingPrivate(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	a.s.Log.FlushCounts()
	id := standin.NewInviteID()
	f := friendsOf(t, maya)["june"]
	box, _ := maya.Seal(f.Key, id, []byte(gameName))
	maya.Invite(id, f.ID, box)
	nextEvent(t, june, "invite")
	june.Join(id)
	maya.Report(id, "failed", "no_path")
	maya.End(id)
	stranger, _ := standin.New(a.address)
	stranger.Pair("K7MR-4QXT-9HPV", "a", "a")
	a.s.Log.FlushCounts()
	for _, b := range []*box2{{a}, {c}} {
		data, err := os.ReadFile(filepath.Join(b.b.dir, "log.txt"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, word := range []string{"maya", "june", "Riley", "Alex's", gameName, "127.0.0.1", "K7MR", id, secret, "acct-"} {
			if strings.Contains(text, word) {
				t.Errorf("%q is in the log:\n%s", word, text)
			}
		}
		t.Log(text)
	}
}

type box2 struct{ b *box }

// A game that goes through the relay: a real TURN allocation with a session's password, bytes counted
// for the month, and the game counted once the consoles say it was relayed.
func TestARelayedGame(t *testing.T) {
	b := startServer(t, "Ours")
	b.form("/relay/on")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")
	if relay := b.get("/relay"); !strings.Contains(relay, "Used this month") {
		t.Fatal(relay)
	}
	id := standin.NewInviteID()
	friend := friendsOf(t, alex)["sam"]
	if _, err := alex.Invite(id, friend.ID, "sealed"); err != nil {
		t.Fatal(err)
	}
	if creds, _, err := alex.Relay(id); err == nil && creds != nil {
		t.Error("no relay before a join")
	}
	if _, err := sam.Join(id); err != nil {
		t.Fatal(err)
	}
	creds, why, err := alex.Relay(id)
	if err != nil || creds == nil {
		t.Fatalf("%q %v", why, err)
	}
	if !strings.HasPrefix(creds.URLs[0], "turn:127.0.0.1:") {
		t.Errorf("%+v", creds)
	}
	if err := alex.Report(id, "relay", "none"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.get("/relay"), "1 game") {
		t.Error("relayed games this month")
	}
	if st := b.s.Store.Snapshot(); st.Games.Relayed != 1 {
		t.Errorf("%+v", st.Games)
	}
	if strings.Contains(b.get("/status"), ">0<") && !strings.Contains(b.get("/status"), "0% direct") {
		t.Error("the Status line should count it")
	}
	if err := sam.End(id); err != nil {
		t.Fatal(err)
	}
}

func TestRelayOffOrFullIsSaid(t *testing.T) {
	b := startServer(t, "Ours")
	alex := player(t, b.address, b.ownCode(), "alex")
	sam := player(t, b.address, b.invite("alex"), "sam")
	id := standin.NewInviteID()
	alex.Invite(id, friendsOf(t, alex)["sam"].ID, "sealed")
	sam.Join(id)
	creds, why, err := alex.Relay(id)
	if err != nil || creds != nil || why != "off" {
		t.Errorf("%v %q %v", creds, why, err)
	}
}

// The host's server's relay first, then the friend's: across a link, the friend's console gets the
// same relay as the host's.
func TestRelayChoiceAcrossALink(t *testing.T) {
	a, c, maya, june := twoLinked(t)
	id := standin.NewInviteID()
	maya.Invite(id, friendsOf(t, maya)["june"].ID, "sealed")
	nextEvent(t, june, "invite")
	if _, err := june.Join(id); err != nil {
		t.Fatal(err)
	}
	if creds, why, _ := june.Relay(id); creds != nil || why == "" {
		t.Errorf("both off: %v %q", creds, why)
	}
	c.form("/relay/on")
	eventually(t, "the friend's relay is used when the host's is off", func() bool {
		creds, _, _ := june.Relay(id)
		return creds != nil
	})
	viaFriend, _, _ := maya.Relay(id)
	a.form("/relay/on")
	eventually(t, "the host's relay comes first", func() bool {
		creds, _, _ := june.Relay(id)
		return creds != nil && creds.URLs[0] != ""
	})
	hostPort := func(creds *standin.RelayCreds) string { return creds.URLs[0] }
	first, _, _ := june.Relay(id)
	second, _, _ := maya.Relay(id)
	if first == nil || second == nil || hostPort(first) != hostPort(second) {
		t.Errorf("both consoles should get the host's relay: %+v %+v", first, second)
	}
	if viaFriend != nil && hostPort(viaFriend) == hostPort(second) {
		t.Error("it should have moved to the host's")
	}
}
