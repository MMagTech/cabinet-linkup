package admin

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

type fakePeople struct {
	online  map[string]bool
	changed []string
	removed []int
}

func (f *fakePeople) Online(id string) bool                 { return f.online[id] }
func (f *fakePeople) FriendsChanged(people ...string)       { f.changed = append(f.changed, people...) }
func (f *fakePeople) ConsoleRemoved(number int, _ []string) { f.removed = append(f.removed, number) }
func (f *fakePeople) EndBetween(string, string)             {}

type fakeLinks struct {
	state   LinkState
	enter   error
	entered []string
	changed int
	pairs   []string
	unlink  []string
}

func (f *fakeLinks) Enter(address, code, person string) error {
	f.entered = append(f.entered, address+"|"+code+"|"+person)
	return f.enter
}
func (f *fakeLinks) RemovePair(link, pair string) error { f.pairs = append(f.pairs, pair); return nil }
func (f *fakeLinks) Unlink(link string) error           { f.unlink = append(f.unlink, link); return nil }
func (f *fakeLinks) Changed()                           { f.changed++ }
func (f *fakeLinks) State(string) LinkState             { return f.state }

type fakeRelay struct {
	status  relay.Status
	applied int
	test    error
}

func (f *fakeRelay) Apply()                       { f.applied++ }
func (f *fakeRelay) Status() relay.Status         { return f.status }
func (f *fakeRelay) Test() (time.Duration, error) { return 38 * time.Millisecond, f.test }

type fakeUpdates struct{ latest string }

func (f fakeUpdates) Latest() string { return f.latest }

type rig struct {
	t       *testing.T
	app     *App
	h       http.Handler
	people  *fakePeople
	links   *fakeLinks
	relay   *fakeRelay
	store   *store.Store
	cookies []*http.Cookie
	remote  string
}

func newRig(t *testing.T) *rig {
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
	r := &rig{t: t, store: st, people: &fakePeople{online: map[string]bool{}}, links: &fakeLinks{state: LinkState{Reachable: true}}, remote: "192.168.1.5:5000"}
	r.relay = &fakeRelay{status: relay.Status{Resets: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), LimitBytes: 500e9}}
	app, err := New(Deps{Store: st, Log: log, Version: "1.2.3", People: r.people, Links: r.links, Relay: r.relay, Updates: fakeUpdates{}})
	if err != nil {
		t.Fatalf("Go refused the templates: %v", err)
	}
	r.app, r.h = app, app.Handler()
	return r
}

func (r *rig) do(method, path string, form url.Values, header ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, "http://192.168.1.10:8080"+path, body)
	req.RemoteAddr = r.remote
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i] == "Host" {
			req.Host = header[i+1]
			continue
		}
		req.Header.Set(header[i], header[i+1])
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			r.cookies = nil
		} else {
			r.cookies = []*http.Cookie{c}
		}
	}
	return rec
}

func (r *rig) get(path string) *httptest.ResponseRecorder { return r.do("GET", path, nil) }

func (r *rig) post(path string, kv ...string) *httptest.ResponseRecorder {
	form := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	return r.do("POST", path, form)
}

func goesTo(t *testing.T, rec *httptest.ResponseRecorder, where string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != where {
		t.Fatalf("got %d to %q, want %q", rec.Code, rec.Header().Get("Location"), where)
	}
}

func shows(t *testing.T, rec *httptest.ResponseRecorder, words ...string) {
	t.Helper()
	body := rec.Body.String()
	if strings.Contains(body, "ZgotmplZ") || strings.Contains(body, "no value") {
		t.Errorf("a template drew nonsense: %s", body)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	for _, w := range words {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body)
		}
	}
}

// setUp walks first run with the relay and own console skipped.
func (r *rig) setUp() {
	r.t.Helper()
	r.post("/setup/password", "password", "correct horse", "again", "correct horse")
	r.post("/setup/address", "name", "Our server", "address", "linkup.example.net:7845")
	r.get("/setup/relay/skip")
	r.get("/setup/own/skip")
}

func (r *rig) fill() {
	r.t.Helper()
	seen := time.Date(2026, 11, 1, 23, 40, 0, 0, time.UTC)
	err := r.store.Update(func(st *store.State) error {
		st.Consoles = []store.Console{{Number: 1}, {Number: 2}, {Number: 3}}
		st.People = []store.Person{
			{ID: "a1", Console: 1, Account: "1", Username: "admin", LastSeen: seen},
			{ID: "a2", Console: 1, Account: "2", Username: "maya", Note: "My daughter", LastSeen: seen},
			{ID: "a3", Console: 2, Account: "1", Username: "alex", Note: "From college", LastSeen: seen},
			{ID: "a4", Console: 3, Account: "1", Username: "admin", LastSeen: seen},
		}
		st.AddFriends("a1", "a3")
		st.Links = []store.Link{{ID: "l1", Number: 1, Name: "Riley's", Address: "linkup.riley.example.org:7845", Failing: seen,
			Pairs: []store.Pair{{ID: "p1", Ours: "a1", Theirs: "x", TheirName: "riley"}, {ID: "p2", Ours: "a2", Theirs: "y", TheirName: "june"}}}}
		st.Codes = []store.Code{
			{ID: "c1", Kind: store.CodeInvite, Code: "K7MR-4QXT-9HPV", Note: "Jordan", For: "a1", Expires: time.Now().Add(time.Hour)},
			{ID: "c2", Kind: store.CodeLink, Code: "T3WN-8FJY-6CRE", Note: "For maya", For: "a2", Expires: time.Now().Add(time.Hour)},
			{ID: "c3", Kind: store.CodeConsole, Code: "H9KD-2MXA-5VPW", Note: "New console code", Console: 2, Person: "a3", Expires: time.Now().Add(time.Hour)},
			{ID: "c4", Kind: store.CodeOwn, Code: "B4ZG-7RQN-3WTF", Note: "Your own console", Expires: time.Now().Add(time.Hour)},
		}
		st.Games = store.Games{Direct: 51, Relayed: 7, Failed: 2}
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func TestFirstRunStartToFinish(t *testing.T) {
	r := newRig(t)
	goesTo(t, r.get("/"), "/setup/password")
	goesTo(t, r.get("/status"), "/setup/password")
	goesTo(t, r.get("/signin"), "/setup/password")
	shows(t, r.get("/setup/password"), "Create admin account", "Step 1 of 4")

	shows(t, r.post("/setup/password", "password", "short", "again", "short"), "Too short")
	shows(t, r.post("/setup/password", "password", "correct horse", "again", "different"), "Doesn&#39;t match")
	goesTo(t, r.post("/setup/password", "password", "correct horse", "again", "correct horse"), "/")
	if r.store.AdminHash() == "" || strings.Contains(r.store.AdminHash(), "correct") {
		t.Fatalf("hash %q", r.store.AdminHash())
	}

	goesTo(t, r.get("/"), "/setup/address")
	goesTo(t, r.get("/status"), "/setup/address")
	shows(t, r.get("/setup/address"), "Step 2 of 4", "Public address")
	shows(t, r.post("/setup/address", "name", "", "address", ""), "Required")
	shows(t, r.post("/setup/address", "name", strings.Repeat("n", 21), "address", "x"), "Too long")
	goesTo(t, r.post("/setup/address", "name", "Our server", "address", "linkup.example.net:7845"), "/")

	goesTo(t, r.get("/"), "/setup/relay")
	shows(t, r.get("/setup/relay"), "Step 3 of 4", `value="500"`, "Skip")
	goesTo(t, r.post("/setup/relay", "gb", "200"), "/")
	if r.relay.applied != 1 {
		t.Error("the relay must start")
	}
	r.store.View(func(st *store.State) {
		if !st.Settings.Relay || st.Settings.RelayLimitGB != 200 {
			t.Errorf("%+v", st.Settings)
		}
	})

	goesTo(t, r.get("/"), "/setup/own")
	shows(t, r.get("/setup/own"), "Step 4 of 4", "Server's local address")
	shows(t, r.post("/setup/own", "address", ""), "Required")
	rec := r.post("/setup/own", "address", "192.168.1.10:7845")
	if rec.Code != http.StatusSeeOther {
		t.Fatal(rec.Code)
	}
	shows(t, r.get(rec.Header().Get("Location")), "Type these on your console", "192.168.1.10:7845", "Settings, Streaming, LinkUp, Server, Add a server")
	goesTo(t, r.get("/"), "/status")
	shows(t, r.get("/status"), "Status")
	shows(t, r.get("/people"), "Your own console", "Invites waiting")
}

// Someone who walks away at any step comes back at the first unfinished one, after signing in.
func TestWalkingAwayResumes(t *testing.T) {
	r := newRig(t)
	r.post("/setup/password", "password", "correct horse", "again", "correct horse")

	away := func(want string) {
		t.Helper()
		r.cookies = nil
		goesTo(t, r.get("/"), "/signin")
		goesTo(t, r.post("/signin", "password", "correct horse"), "/")
		goesTo(t, r.get("/"), want)
	}
	away("/setup/address")
	r.post("/setup/address", "name", "Our server", "address", "x:1")
	away("/setup/relay")
	goesTo(t, r.get("/setup/relay/skip"), "/")
	away("/setup/own")
	r.store.View(func(st *store.State) {
		if st.Settings.Relay {
			t.Error("a Skip leaves the relay off")
		}
	})
	goesTo(t, r.get("/setup/own/skip"), "/status")
	away("/status")
	goesTo(t, r.get("/setup/address"), "/")
	goesTo(t, r.get("/setup/relay"), "/")
}

// Every screen, drawn by real Go, with every kind of thing on it.
func TestEveryScreenDraws(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	r.people.online["a1"] = true
	r.relay.status.On = true
	r.relay.status.UsedBytes = 123e9
	r.relay.status.Found = "203.0.113.24:3478"
	r.relay.status.Games = 7

	for path, words := range map[string][]string{
		"/status":                         {"Status", "1 of 4", "58, 87% direct", "123", "December 1"},
		"/status/games":                   {"Games", "Played", ">58<", ">51<", ">7<", ">2<"},
		"/people":                         {"People", "admin", "maya", "My daughter", "alex", "Jordan", "New console code"},
		"/people?person=a1":               {"Friends", "alex", "riley", "Riley&#39;s", "Online", "Remove console", "New console code"},
		"/people?person=a2":               {"My daughter", "june", "Last seen"},
		"/people/add":                     {"Invite a friend", "Friend of", "admin, console 1", "admin, console 3", "maya"},
		"/people/code?id=c1":              {"Invite ready", "Jordan", "K7MR-4QXT-9HPV", "linkup.example.net:7845", "Let&#39;s play on CabinetOS with LinkUp.", "Cancel invite", "Copy"},
		"/people/code?id=c3&reinstall=a3": {"New console code", "H9KD-2MXA-5VPW", "Here&#39;s a new LinkUp code for your console."},
		"/people/code?id=c4":              {"Your console", "B4ZG-7RQN-3WTF", "The code for my own console."},
		"/consoles/remove?id=1&person=a1": {"Remove admin and maya?"},
		"/relay":                          {"Relay", "On", "123", "of 500 GB", "7 games, resets", "December 1", "203.0.113.24:3478", "Test"},
		"/server":                         {"Server", "Our server", "linkup.example.net:7845", "Linked servers", "Settings"},
		"/server/settings":                {"Settings", "Password", "1.2.3", "Graphite", "Midnight", "Light", "CabinetOS", "Sign out"},
		"/server/stun":                    {"STUN", "stun.cloudflare.com:3478", "Reset"},
		"/server/log":                     {"Log", "Download"},
		"/password":                       {"Change password", "Current password", "New password", "Confirm password"},
		"/links":                          {"Linked servers", "Riley&#39;s", "Create code", "Enter code", "Codes waiting", "For maya"},
		"/links?server=l1":                {"This server", "riley", "june", "Add friends", "Unlink", "Connected"},
		"/links/make":                     {"Create link code", "Account on this server"},
		"/links/enter":                    {"Enter link code", "Server address", "Link servers"},
		"/links/code?id=c2":               {"Link code", "For maya", "This code is for maya.", "T3WN-8FJY-6CRE", "Server, Linked servers, Enter code."},
		"/links/remove?id=l1":             {"Unlink Riley&#39;s?"},
	} {
		t.Run(path, func(t *testing.T) { shows(t, r.get(path), words...) })
	}
}

func TestEmptyServerDraws(t *testing.T) {
	r := newRig(t)
	r.setUp()
	for path, words := range map[string][]string{
		"/status":       {">0<", "0"},
		"/status/games": {"Played"},
		"/people":       {"No people", "Add your console"},
		"/people/add":   {"Invite a friend"},
		"/relay":        {"Off"},
		"/links":        {"Create code"},
		"/links/make":   {"Create link code", "Pair a console first."},
		"/links/enter":  {"Enter link code", "Pair a console first."},
		"/server/log":   {"No entries"},
	} {
		t.Run(path, func(t *testing.T) { shows(t, r.get(path), words...) })
	}
	if strings.Contains(r.get("/people/add").Body.String(), "Friend of") {
		t.Error("Friend of only when someone is there to pick")
	}
	for _, path := range []string{"/links/make", "/links/enter"} {
		if strings.Contains(r.get(path).Body.String(), `action="`+path+`"`) {
			t.Errorf("%s: no form while there is nobody to link", path)
		}
	}
	goesTo(t, r.post("/links/make", "for", ""), "/links/make")
	shows(t, r.post("/links/enter", "address", "x:1", "code", "GOOD"), "Pair a console first.")
	if len(r.links.entered) != 0 {
		t.Error("nothing is sent to the other server without an account to link")
	}
	if strings.Contains(r.get("/relay").Body.String(), "Used this month") {
		t.Error("off shows only the switch")
	}
}

func TestLinkProblemsDraw(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	r.links.state = LinkState{Reachable: false}
	shows(t, r.get("/status"), "Can&#39;t reach Riley&#39;s")
	shows(t, r.get("/links"), "Can't reach")
	shows(t, r.get("/links?server=l1"), "Can't reach since", "2026-11-01")
	r.store.Update(func(st *store.State) error { st.Links[0].Failing = time.Time{}; return nil })
	body := r.get("/links?server=l1")
	shows(t, body, "Not reached yet")
	if strings.Contains(body.Body.String(), "Can't reach since") {
		t.Error("a link never reached has no since")
	}
	r.links.state = LinkState{Reachable: true, BehindTheirs: true}
	shows(t, r.get("/status"), "Riley&#39;s needs an update")
	shows(t, r.get("/links"), "Needs update")
	r.links.state = LinkState{Reachable: true, BehindOurs: true}
	shows(t, r.get("/links?server=l1"), "This server needs an update")
}

func TestRelayStatesDraw(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.relay.status.On = true
	r.relay.status.LimitBytes = 100e9
	r.relay.status.UsedBytes = 95e9
	shows(t, r.get("/relay"), "warn", "95", "Not found")
	r.relay.status.AtLimit = true
	r.relay.status.UsedBytes = 100e9
	shows(t, r.get("/relay"), "Limit reached, resets December 1")
	shows(t, r.get("/status"), "Relay limit reached")
	r.relay.status.AtLimit = false
	r.relay.status.UsedBytes = 1.4e9
	shows(t, r.get("/relay"), "1.4")
	r.relay.status.TestFailed = true
	shows(t, r.get("/relay"), "Test failed")
}

func TestRelayForms(t *testing.T) {
	r := newRig(t)
	r.setUp()
	goesTo(t, r.post("/relay/on"), "/relay")
	r.store.View(func(st *store.State) {
		if !st.Settings.Relay {
			t.Error("on")
		}
	})
	if r.relay.applied != 1 {
		t.Error("apply")
	}
	goesTo(t, r.post("/relay/limit", "gb", "50", "day", "40"), "/relay")
	r.store.View(func(st *store.State) {
		if st.Settings.RelayLimitGB != 50 || st.Settings.RelayResetDay != 31 {
			t.Errorf("%+v", st.Settings)
		}
	})
	goesTo(t, r.post("/relay/limit", "gb", "0", "day", "nonsense"), "/relay")
	r.store.View(func(st *store.State) {
		if st.Settings.RelayLimitGB != 50 || st.Settings.RelayResetDay != 31 {
			t.Errorf("bad values must change nothing: %+v", st.Settings)
		}
	})
	r.relay.status.On = true
	goesTo(t, r.post("/relay/test"), "/relay")
	shows(t, r.get("/relay"), "Test passed, 38 ms")
	r.relay.test = errors.New("no")
	goesTo(t, r.post("/relay/test"), "/relay")
	if strings.Contains(r.get("/relay").Body.String(), "Test passed") {
		t.Error("a toast is for what went fine")
	}
	goesTo(t, r.post("/relay/off"), "/relay")
}

func TestInvitesAndCodes(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	rec := r.post("/people/add", "note", strings.Repeat("n", 90), "for", "a2")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/people/code?id=") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Location"))
	}
	var made store.Code
	r.store.View(func(st *store.State) { made = st.Codes[len(st.Codes)-1] })
	if len([]rune(made.Note)) != store.MaxNoteLength || made.For != "a2" || len(made.K) != 32 || made.Code == "" {
		t.Errorf("%+v", made)
	}
	if d := time.Until(made.Expires); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("expires in %v", d)
	}
	goesTo(t, r.post("/people/codes/cancel", "code", made.ID), "/people")
	shows(t, r.get("/people"), "Invite cancelled")
	r.store.View(func(st *store.State) {
		if st.Code(made.ID) != nil {
			t.Error("not cancelled")
		}
	})
	goesTo(t, r.post("/people/add", "note", "x", "for", "nobody"), "/people/add")

	rec = r.post("/consoles/new-code", "id", "2", "person", "a3")
	if !strings.Contains(rec.Header().Get("Location"), "reinstall=a3") {
		t.Fatal(rec.Header().Get("Location"))
	}
	shows(t, r.get(rec.Header().Get("Location")), "New console code")

	r.store.Update(func(st *store.State) error { st.Codes[0].Expires = time.Now().Add(-time.Minute); return nil })
	r.get("/people")
	r.store.View(func(st *store.State) {
		if st.Code("c1") != nil {
			t.Error("an expired code should be gone")
		}
	})
}

func TestNotesAndFriends(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	goesTo(t, r.post("/people/note", "person", "a3", "note", "Alex"), "/people?person=a3")
	shows(t, r.get("/people?person=a3"), "Saved", `value="Alex"`)
	goesTo(t, r.post("/friends/add", "person", "a2", "friend", "a3"), "/people?person=a2")
	r.store.View(func(st *store.State) {
		if !st.AreFriends("a2", "a3") {
			t.Error("not friends")
		}
	})
	if len(r.people.changed) != 2 {
		t.Errorf("both consoles should hear: %v", r.people.changed)
	}
	if strings.Contains(r.get("/people?person=a2").Body.String(), `<option value="a3">`) {
		t.Error("a friend is not offered again")
	}
	goesTo(t, r.post("/friends/remove", "person", "a2", "id", "f:a3"), "/people?person=a2")
	r.store.View(func(st *store.State) {
		if st.AreFriends("a2", "a3") {
			t.Error("still friends")
		}
	})
	goesTo(t, r.post("/friends/remove", "person", "a1", "id", "p:l1:p1"), "/people?person=a1")
	if len(r.links.pairs) != 1 || r.links.pairs[0] != "p1" {
		t.Errorf("%v", r.links.pairs)
	}
}

func TestRemoveConsole(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	goesTo(t, r.post("/consoles/remove", "id", "2"), "/people")
	r.store.View(func(st *store.State) {
		if st.Person("a3") != nil || !st.ConsoleByNumber(2).Removed || st.AreFriends("a1", "a3") {
			t.Errorf("%+v", st)
		}
		if st.Code("c3") != nil {
			t.Error("its new code should go too")
		}
	})
	if len(r.people.removed) != 1 || r.people.removed[0] != 2 {
		t.Errorf("%v", r.people.removed)
	}
}

func TestEnteringALinkCode(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	shows(t, r.post("/links/enter", "address", "", "code", ""), "Required")
	shows(t, r.post("/links/enter", "address", "x:1", "code", "GOOD", "for", "nobody"), "Required", `value="GOOD"`)
	if len(r.links.entered) != 0 {
		t.Error("an account that is not here is not a wrong code, and nothing is sent")
	}
	r.links.enter = store.ErrWrongCode
	shows(t, r.post("/links/enter", "address", "x:1", "code", "BAD", "for", "a1"), "Wrong or expired", `value="x:1"`, `value="BAD"`)
	r.links.enter = store.ErrCantReach
	shows(t, r.post("/links/enter", "address", "x:1", "code", "BAD", "for", "a1"), "Can&#39;t reach")
	r.links.enter = nil
	goesTo(t, r.post("/links/enter", "address", "x:1", "code", "GOOD", "for", "a1"), "/links")
	if got := r.links.entered[len(r.links.entered)-1]; got != "x:1|GOOD|a1" {
		t.Error(got)
	}
	goesTo(t, r.post("/links/pairs/remove", "pair", "p2"), "/links?server=l1")
	goesTo(t, r.post("/links/remove", "id", "l1"), "/links")
	if len(r.links.unlink) != 1 {
		t.Error("unlink")
	}
}

func TestServerForm(t *testing.T) {
	r := newRig(t)
	r.setUp()
	shows(t, r.post("/server/about", "name", "", "address", "new:1"), "Required", `value="new:1"`)
	shows(t, r.post("/server/about", "name", "x", "address", "bad\nline"), "Not valid")
	for _, address := range []string{"linkup.example.net:7845", "203.0.113.5:7845", "[2001:db8::1]:7845", "linkup.example.net", "https://proxy.example.net/linkup", "name.tailnet.ts.net:7845", "100.100.1.2:7845"} {
		goesTo(t, r.post("/server/about", "name", "Ours", "address", address), "/server")
		r.store.View(func(st *store.State) {
			if st.Settings.Address != address {
				t.Errorf("free text must stay as typed: %q", st.Settings.Address)
			}
		})
	}
	if r.links.changed == 0 {
		t.Error("linked servers should hear")
	}
}

func TestThemeIsSavedOnTheServerAndOnEveryPage(t *testing.T) {
	r := newRig(t)
	r.setUp()
	if strings.Contains(r.get("/status").Body.String(), "/static/theme-") {
		t.Error("CabinetOS is the default, with no extra file")
	}
	goesTo(t, r.post("/server/theme", "theme", "midnight"), "/server/settings")
	for _, path := range []string{"/status", "/people", "/relay", "/server/settings"} {
		shows(t, r.get(path), "theme-midnight.css", `content="#0c1428"`)
	}
	r.cookies = nil
	shows(t, r.get("/signin"), "theme-midnight.css")
	r.post("/signin", "password", "correct horse")
	goesTo(t, r.post("/server/theme", "theme", "../../etc"), "/server/settings")
	shows(t, r.get("/status"), "theme-midnight.css")
}

func TestStunList(t *testing.T) {
	r := newRig(t)
	r.setUp()
	list := func() []store.Stun { return r.store.Snapshot().Settings.Stun }
	goesTo(t, r.post("/stun/add", "address", "stun.example.net:3478"), "/server/stun")
	if len(list()) != 3 || list()[2].Address != "stun.example.net:3478" {
		t.Fatalf("%+v", list())
	}
	r.post("/stun/up", "id", list()[2].ID)
	if list()[1].Address != "stun.example.net:3478" {
		t.Errorf("up: %+v", list())
	}
	r.post("/stun/down", "id", list()[1].ID)
	if list()[2].Address != "stun.example.net:3478" {
		t.Errorf("down: %+v", list())
	}
	r.post("/stun/remove", "id", list()[0].ID)
	r.post("/stun/remove", "id", list()[0].ID)
	r.post("/stun/remove", "id", list()[0].ID)
	if len(list()) != 0 {
		t.Errorf("the defaults can be removed: %+v", list())
	}
	shows(t, r.get("/server/stun"), "STUN")
	r.post("/stun/reset")
	if len(list()) != 2 || list()[0].Address != store.DefaultStun[0] {
		t.Errorf("reset: %+v", list())
	}
}

func TestPasswordChange(t *testing.T) {
	r := newRig(t)
	r.setUp()
	other := *r
	other.cookies = nil
	other.post("/signin", "password", "correct horse")
	shows(t, r.post("/password", "current", "wrong", "password", "new password", "again", "new password"), "Incorrect")
	shows(t, r.post("/password", "current", "correct horse", "password", "new", "again", "new"), "Too short")
	shows(t, r.post("/password", "current", "correct horse", "password", "new password", "again", "different"), "Doesn&#39;t match")
	goesTo(t, r.post("/password", "current", "correct horse", "password", "new password", "again", "new password"), "/server/settings")
	goesTo(t, other.get("/status"), "/signin")
	shows(t, r.get("/server/settings"), "Saved")
	r.cookies = nil
	shows(t, r.post("/signin", "password", "correct horse"), "Incorrect")
	goesTo(t, r.post("/signin", "password", "new password"), "/")
}

func TestSignOutAndWrongPasswords(t *testing.T) {
	r := newRig(t)
	r.setUp()
	goesTo(t, r.post("/signout"), "/signin")
	goesTo(t, r.get("/status"), "/signin")
	for range signinTries {
		shows(t, r.post("/signin", "password", "wrong"), "Incorrect")
	}
	rec := r.post("/signin", "password", "correct horse")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many tries") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// Each try is counted before its slow hash. Counted after, every try sent at once passes the count
// while the hash makes it wait, and a browser could check as many as it liked.
func TestManySigninsAtOnceStillCount(t *testing.T) {
	r := newRig(t)
	r.setUp()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r.app.sessions.nowFn = func() time.Time { return now }
	signin := func(password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://192.168.1.10:8080/signin", strings.NewReader(url.Values{"password": {password}}.Encode()))
		req.RemoteAddr = r.remote
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		return rec
	}
	var wg sync.WaitGroup
	var checked, refused atomic.Int32
	for range 4 * signinTries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch rec := signin("wrong guess"); {
			case rec.Code == http.StatusTooManyRequests:
				refused.Add(1)
			case strings.Contains(rec.Body.String(), "Incorrect"):
				checked.Add(1)
			}
		}()
	}
	wg.Wait()
	if checked.Load() != signinTries || refused.Load() != 3*signinTries {
		t.Fatalf("%d tries at once: %d checked and %d refused, want %d checked", 4*signinTries, checked.Load(), refused.Load(), signinTries)
	}
	if rec := signin("correct horse"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the right password inside the minute got %d", rec.Code)
	}
	now = now.Add(signinWindow + time.Second)
	for range 3 {
		// A right password gives its try back, so the owner is never counted against.
		if rec := signin("correct horse"); rec.Code != http.StatusSeeOther {
			t.Fatalf("the right password after the minute got %d", rec.Code)
		}
	}
	for range signinTries {
		shows(t, signin("wrong"), "Incorrect")
	}
}

// Only a missing admin.json opens first run. One the server cannot read keeps the page locked, so a
// passing fault or a damaged file cannot hand the password to whoever asks next.
func TestADamagedPasswordFileDoesNotReopenFirstRun(t *testing.T) {
	r := newRig(t)
	r.setUp()
	if err := os.WriteFile(filepath.Join(r.store.Dir(), "admin.json"), []byte(`{"Hash": "$argon2id$v=19$m=65536`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cookies = nil
	goesTo(t, r.get("/status"), "/signin")
	goesTo(t, r.post("/setup/password", "password", "the strangers", "again", "the strangers"), "/")
	goesTo(t, r.get("/status"), "/signin")
	shows(t, r.post("/signin", "password", "the strangers"), "Incorrect")
}

func TestForgottenPasswordIsDeletingTheFile(t *testing.T) {
	r := newRig(t)
	r.setUp()
	if err := osRemove(filepath.Join(r.store.Dir(), "admin.json")); err != nil {
		t.Fatal(err)
	}
	goesTo(t, r.get("/status"), "/setup/password")
	goesTo(t, r.post("/setup/password", "password", "another one", "again", "another one"), "/")
	goesTo(t, r.get("/"), "/status")
}

func TestPublicAddressesRefused(t *testing.T) {
	r := newRig(t)
	for remote, allowed := range map[string]bool{
		"8.8.8.8:1":              false,
		"203.0.113.9:1":          false,
		"[2606:4700::1]:1":       false,
		"[::ffff:8.8.8.8]:1":     false,
		"127.0.0.1:1":            true,
		"[::1]:1":                true,
		"192.168.0.2:1":          true,
		"10.1.2.3:1":             true,
		"172.16.0.9:1":           true,
		"100.64.0.1:1":           true,
		"100.127.255.254:1":      true,
		"[fd12::1]:1":            true,
		"[fe80::1]:1":            true,
		"[::ffff:192.168.0.2]:1": true,
		"not an address":         false,
	} {
		r.remote = remote
		rec := r.get("/setup/password")
		if allowed && rec.Code != http.StatusOK {
			t.Errorf("%s got %d", remote, rec.Code)
		}
		if !allowed && (rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only on your home network")) {
			t.Errorf("%s got %d", remote, rec.Code)
		}
	}
	r.remote = "8.8.8.8:1"
	rec := r.post("/setup/password", "password", "correct horse", "again", "correct horse")
	if rec.Code != http.StatusForbidden || r.store.AdminHash() != "" {
		t.Error("a public address must not be able to set the password")
	}
}

func TestReverseProxyHeadersCanOnlyRefuse(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		name, value string
		ok          bool
	}{
		{"X-Forwarded-For", "8.8.8.8", false},
		{"X-Forwarded-For", "192.168.1.9, 8.8.8.8", false},
		{"X-Forwarded-For", "192.168.1.9", true},
		{"X-Real-Ip", "8.8.8.8", false},
		{"Forwarded", "for=8.8.8.8;proto=https", false},
		{"Forwarded", `for="[2606:4700::1]:443"`, false},
		{"Forwarded", "for=192.168.1.9", true},
	} {
		rec := r.do("GET", "/setup/password", nil, c.name, c.value)
		if c.ok != (rec.Code == http.StatusOK) {
			t.Errorf("%s: %s got %d", c.name, c.value, rec.Code)
		}
	}
	r.remote = "8.8.8.8:1"
	if rec := r.do("GET", "/setup/password", nil, "X-Forwarded-For", "192.168.1.9"); rec.Code != http.StatusForbidden {
		t.Error("a header must not be able to open it")
	}
}

// A web page a browser at home has open can point a public name of its own at the server (DNS
// rebinding), and to that browser it is then the server's own page, sending from a home address. So
// first run takes only a name no stranger can point: an address, a name with no dots, or a home
// suffix. Once the password is set, any name works, because signing in needs it.
func TestFirstRunAsksForAHomeName(t *testing.T) {
	r := newRig(t)
	for host, home := range map[string]bool{
		"rebind.attacker.example:7846": false,
		"linkup.example.net":           false,
		"192.168.1.10:7846":            true,
		"[fd12::5]:7846":               true,
		"localhost:7846":               true,
		"tower:7846":                   true,
		"tower.local:7846":             true,
		"nas.home.arpa":                true,
		"TOWER.LAN.:7846":              true,
	} {
		rec := r.do("GET", "/setup/password", nil, "Host", host)
		if home && rec.Code != http.StatusOK {
			t.Errorf("%s got %d", host, rec.Code)
		}
		if !home && (rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Open it by its address")) {
			t.Errorf("%s got %d", host, rec.Code)
		}
	}
	form := url.Values{"password": {"the strangers"}, "again": {"the strangers"}}
	if rec := r.do("POST", "/setup/password", form, "Host", "rebind.attacker.example:7846"); rec.Code != http.StatusForbidden || r.store.AdminHash() != "" {
		t.Fatal("a page under a public name must not set the password")
	}
	r.setUp()
	r.cookies = nil
	if rec := r.do("GET", "/signin", nil, "Host", "linkup-admin.example.net"); rec.Code != http.StatusOK {
		t.Errorf("signing in under the owner's own name got %d", rec.Code)
	}
}

func TestFormsFromAnotherSiteRefused(t *testing.T) {
	r := newRig(t)
	r.setUp()
	form := url.Values{"theme": {"light"}}
	for _, h := range [][]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		if rec := r.do("POST", "/server/theme", form, h...); rec.Code != http.StatusForbidden {
			t.Errorf("%v got %d", h, rec.Code)
		}
	}
	if rec := r.do("POST", "/server/theme", form, "Origin", "http://192.168.1.10:8080"); rec.Code != http.StatusSeeOther {
		t.Errorf("own origin got %d", rec.Code)
	}
}

func TestCookieAndHeaders(t *testing.T) {
	r := newRig(t)
	rec := r.post("/setup/password", "password", "correct horse", "again", "correct horse")
	c := rec.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("%+v", c)
	}
	rec = r.get("/setup/address")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a page must not be cached")
	}
	for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(name) == "" {
			t.Errorf("no %s", name)
		}
	}
	if strings.Contains(rec.Header().Get("Referrer-Policy"), "no-referrer") {
		t.Error("no-referrer would make a plain-HTTP form send an Origin of null")
	}
}

func TestStaticFiles(t *testing.T) {
	r := newRig(t)
	for name, kind := range map[string]string{
		"NotoSans-latin.woff2": "font/woff2",
		"admin.css":            "text/css",
		"admin.js":             "text/javascript",
		"mark.svg":             "image/svg+xml",
		"theme-light.css":      "text/css",
	} {
		rec := r.get("/static/" + name)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), kind) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=31536000") {
			t.Errorf("%s: %s", name, rec.Header().Get("Cache-Control"))
		}
	}
	for _, path := range []string{"/static/", "/static/../admin.go", "/static/nothing.css"} {
		if rec := r.get(path); rec.Code == http.StatusOK {
			t.Errorf("%s served", path)
		}
	}
	r.remote = "8.8.8.8:1"
	if rec := r.get("/static/admin.css"); rec.Code != http.StatusOK {
		t.Error("the refused page needs its own style")
	}
	shows2 := r.get("/status").Body.String()
	if !strings.Contains(shows2, "/static/admin.css?v=1.2.3") {
		t.Error("the page's links must carry the version")
	}
}

func TestLogDownload(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.app.Log.GameDirect()
	shows(t, r.get("/server/log"), "game connected, direct")
	rec := r.get("/log")
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(rec.Body.String(), "game connected, direct") {
		t.Errorf("%+v", rec)
	}
	r.cookies = nil
	goesTo(t, r.get("/log"), "/signin")
}

func TestSignedOutGetsNothing(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	r.cookies = nil
	for _, path := range []string{"/status", "/people", "/people/code?id=c1", "/relay", "/server", "/server/settings", "/server/log", "/log", "/links", "/links/code?id=c2", "/setup/own/code?id=c4"} {
		if rec := r.get(path); rec.Code != http.StatusSeeOther || strings.Contains(rec.Body.String(), "K7MR") {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/people/add", "/server/theme", "/relay/on", "/consoles/remove"} {
		goesTo(t, r.post(path), "/signin")
	}
}

func osRemove(path string) error { return os.Remove(path) }

// One RomM account on two consoles is two people with one name: each says its console wherever it is
// shown, after the note if there is one. A name nobody else has is shown as it was.
func TestSameNameShowsItsConsole(t *testing.T) {
	r := newRig(t)
	r.setUp()
	r.fill()
	r.store.Update(func(st *store.State) error { st.Person("a4").Note = "Upstairs"; return nil })
	r.people.online["a1"] = true
	people := r.get("/people").Body.String()
	for _, want := range []string{`<span class="item-sub">Console 1</span>`, `<span class="item-sub">Console 3 · Upstairs</span>`,
		`<span class="item-sub">My daughter</span>`, `<span class="item-sub">From college</span>`} {
		if !strings.Contains(people, want) {
			t.Errorf("People is missing %q", want)
		}
	}
	if strings.Contains(people, "Console 2") {
		t.Error("alex is the only alex, so alex's console stays off the page")
	}
	shows(t, r.get("/people?person=a1"), "Online · Console 1")
	shows(t, r.get("/people?person=a3"), `<div class="item-name">admin</div><div class="item-sub">Console 1</div>`, ">admin, console 3</option>")
	shows(t, r.get("/links?server=l1"), `<span class="item-name">admin</span><span class="item-sub">Console 1</span>`)
	shows(t, r.get("/people/add"), ">admin, console 1</option>", ">admin, console 3</option>", ">maya</option>")
	goesTo(t, r.post("/links/make", "for", "a4"), "/links/code?id="+r.newestCode())
	shows(t, r.get("/links/code?id="+r.newestCode()), "For admin, console 3", "This code is for admin, console 3.")
}

func (r *rig) newestCode() string {
	var id string
	r.store.View(func(st *store.State) { id = st.Codes[len(st.Codes)-1].ID })
	return id
}

// Games at once, on the Relay tab and in first run, with the one range to forward for them.
func TestGamesAtOnce(t *testing.T) {
	r := newRig(t)
	r.post("/setup/password", "password", "correct horse", "again", "correct horse")
	r.post("/setup/address", "name", "Our server", "address", "linkup.example.net:7845")
	r.relay.status.GamesAtOnce, r.relay.status.FirstPort, r.relay.status.LastPort = 2, 3478, 3482
	shows(t, r.get("/setup/relay"), "Games at once", `name="games" value="2" min="1" max="16"`, `data-games="3478"`,
		`Forward UDP 3478 to <span data-last>3482</span> on your router`)
	goesTo(t, r.post("/setup/relay", "gb", "200", "games", "3"), "/")
	r.store.View(func(st *store.State) {
		if st.Settings.RelayGames != 3 || !st.Settings.Relay {
			t.Errorf("%+v", st.Settings)
		}
	})
	r.get("/setup/own/skip")
	r.relay.status.On = true
	r.relay.status.GamesAtOnce, r.relay.status.LastPort = 3, 3484
	shows(t, r.get("/relay"), "Games at once", `name="games" value="3"`, `Forward UDP 3478 to <span data-last>3484</span> on your router`)
	for _, c := range []struct {
		games string
		want  int
	}{{"40", 16}, {"0", 1}, {"nonsense", 1}, {"5", 5}} {
		goesTo(t, r.post("/relay/limit", "gb", "500", "day", "1", "games", c.games), "/relay")
		r.store.View(func(st *store.State) {
			if st.Settings.RelayGames != c.want {
				t.Errorf("games %q saved as %d, want %d", c.games, st.Settings.RelayGames, c.want)
			}
		})
	}
}
