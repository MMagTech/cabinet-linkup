package admin

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/MMagTech/cabinet-linkup/internal/link"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
	"github.com/MMagTech/cabinet-linkup/internal/update"
)

const tints = 5

type friendView struct {
	ID, Name, Server, Console, Letter string
	Tint                              int
}

type option struct {
	ID, Name string
}

type personView struct {
	ID, Name, Note, Letter, LastSeen string
	Console                          string // 2P Start, their consoles, or which console for a login another person has too
	Tint                             int
	Online, Current                  bool
	Friends                          []friendView
	CanAdd                           []option
	Consoles                         []consoleView
}

// consoleView is one of the consoles a person is signed in on, to look at: the owner manages the
// person, with their code, and never a console by itself.
type consoleView struct {
	Number      int
	Added, Used string
}

type waitingView struct {
	ID, Note, Expires, MessageURL string
}

type peopleView struct {
	People   []personView
	Waiting  []waitingView
	Selected *personView
}

type pairView struct {
	ID, Ours, OursConsole, Theirs string
}

type linkView struct {
	ID, Name, Address, Since                    string
	Reachable, Behind, BehindTheirs, BehindOurs bool
	Current                                     bool
	Pairs                                       []pairView
}

type linksView struct {
	Servers  []linkView
	Waiting  []waitingView
	Selected *linkView
}

type problemView struct {
	Line, URL string
}

type statusView struct {
	RelayOn, Warn                                  bool
	UsedGB, Resets, GamesLine, Update              string
	LimitGB, UsedPercent, PeopleCount, OnlineCount int
	Pending, LinkCount                             int
	Problems                                       []problemView
}

type gamesView struct {
	Games, Direct, Relayed, DirectCount, RelayedCount, Failed int
}

type relayView struct {
	On, Warn, AtLimit, TestFailed         bool
	UsedGB, Resets, Found, GamesMonthText string
	LimitGB, UsedPercent, ResetDay        int
	GamesMonth                            int
	GamesAtOnce, MaxGames                 int
	FirstPort, LastPort                   int
}

type serverView struct {
	Name, Address, Update, UpdateURL string
	ErrorName, ErrorAddress          string
	LinkCount                        int
}

type themeView struct {
	ID, Name string
	Current  bool
}

type settingsView struct {
	Version string
	Themes  []themeView
}

type stunView struct {
	ID, Address string
}

type readyView struct {
	Title, Who, Intro, Steps, Address, Code, Expires string
	CancelURL, CancelWord, ChangeURL, ID, Back       string
}

type confirmView struct {
	Question, Sub, Action, ID, Yes, Back string
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// tintOf works out a person's colour from their name, so nothing is stored.
func tintOf(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32()%tints) + 1
}

func letterOf(name string) string {
	for _, r := range name {
		return string(unicode.ToUpper(r))
	}
	return "?"
}

// A login is not unique: two RomM accounts can have one name, and one account can be two people, when
// its second console came in before consoles joined up. Wherever such a person is shown, their
// consoles tell them apart.
func sharesName(st *store.State, p store.Person) bool {
	for _, o := range st.People {
		if o.ID != p.ID && o.Username == p.Username {
			return true
		}
	}
	return false
}

// signedIn is the consoles a person is signed in on, the one used last first. Change code and
// Remove sign them out of all of them.
func signedIn(st *store.State, p store.Person) []store.Console {
	var out []store.Console
	for _, n := range p.Consoles() {
		if c := st.ConsoleByNumber(n); c != nil && !c.Removed {
			out = append(out, *c)
		}
	}
	return out
}

// consolesText says which consoles a person is on: "Console 2", "2 consoles", or "No consoles".
func consolesText(st *store.State, p store.Person) string {
	on := signedIn(st, p)
	switch len(on) {
	case 0:
		return "No consoles"
	case 1:
		return "Console " + strconv.Itoa(on[0].Number)
	}
	return strconv.Itoa(len(on)) + " consoles"
}

// countLine is the line under a person's name: "2P Start" for someone on an app that only joins,
// then how many consoles they are on when it is not one, or which one when another person has the
// same name, or "".
func countLine(st *store.State, p store.Person) string {
	line := consoleLine(st, p)
	if len(signedIn(st, p)) != 1 {
		line = consolesText(st, p)
	}
	switch {
	case !p.JoinsOnly:
		return line
	case line == "":
		return "2P Start"
	}
	return "2P Start · " + line
}

// joinsOnly picks out the people who never host, for a pick that needs someone who can.
func joinsOnly(p store.Person) bool {
	return p.JoinsOnly
}

// consoleLine is the line under a person's name that tells them apart, or "".
func consoleLine(st *store.State, p store.Person) string {
	if !sharesName(st, p) {
		return ""
	}
	return consolesText(st, p)
}

// nameOf is a person's name on one line, with their consoles if another person has the same name.
func nameOf(st *store.State, p store.Person) string {
	if !sharesName(st, p) {
		return p.Username
	}
	return p.Username + ", " + strings.ToLower(consolesText(st, p))
}

func consoleRows(st *store.State, p store.Person) []consoleView {
	var out []consoleView
	for _, c := range signedIn(st, p) {
		out = append(out, consoleView{Number: c.Number, Added: rfc(c.Paired), Used: rfc(c.LastUsed)})
	}
	return out
}

// accountOptions lists this server's people for a pick.
func accountOptions(st *store.State, skip func(store.Person) bool) []option {
	var out []option
	for _, p := range st.People {
		if skip != nil && skip(p) {
			continue
		}
		out = append(out, option{ID: p.ID, Name: nameOf(st, p)})
	}
	return out
}

func (a *App) person(st *store.State, p store.Person, selected string) personView {
	v := personView{
		ID: p.ID, Name: p.Username, Note: p.Note, Console: consoleLine(st, p), Letter: letterOf(p.Username), Tint: tintOf(p.Username),
		LastSeen: rfc(p.LastSeen), Online: a.People.Online(p.ID), Current: p.ID == selected, Consoles: consoleRows(st, p),
	}
	v.Console = countLine(st, p)
	for _, id := range st.FriendsOf(p.ID) {
		if f := st.Person(id); f != nil {
			v.Friends = append(v.Friends, friendView{ID: "f:" + f.ID, Name: f.Username, Console: consoleLine(st, *f), Letter: letterOf(f.Username), Tint: tintOf(f.Username)})
		}
	}
	for _, l := range st.Links {
		for _, pair := range l.Pairs {
			if pair.Ours == p.ID {
				v.Friends = append(v.Friends, friendView{ID: "p:" + l.ID + ":" + pair.ID, Name: pair.TheirName, Server: l.Name, Letter: letterOf(pair.TheirName), Tint: tintOf(pair.TheirName)})
			}
		}
	}
	v.CanAdd = accountOptions(st, func(o store.Person) bool {
		return o.ID == p.ID || st.AreFriends(o.ID, p.ID) || !store.CanPlay(&o, &p)
	})
	return v
}

func waitingRows(st *store.State, links bool) []waitingView {
	var out []waitingView
	for _, c := range st.Codes {
		if (c.Kind == store.CodeLink) != links || c.Kind == store.CodePerson {
			continue
		}
		url := "/people/code?id=" + c.ID
		if links {
			url = "/links/code?id=" + c.ID
		}
		out = append(out, waitingView{ID: c.ID, Note: c.Note, Expires: rfc(c.Expires), MessageURL: url})
	}
	return out
}

func (a *App) peopleView(selected string) peopleView {
	a.purge()
	st := a.Store.Snapshot()
	v := peopleView{Waiting: waitingRows(&st, false)}
	for _, p := range st.People {
		row := a.person(&st, p, selected)
		v.People = append(v.People, row)
		if p.ID == selected {
			chosen := row
			v.Selected = &chosen
		}
	}
	return v
}

func (a *App) linkRow(st *store.State, l store.Link, selected string) linkView {
	state := a.Links.State(l.ID)
	v := linkView{
		ID: l.ID, Name: l.Name, Address: link.DisplayAddress(l), Since: rfc(l.Failing), Reachable: state.Reachable,
		BehindTheirs: state.BehindTheirs, BehindOurs: state.BehindOurs, Behind: state.BehindTheirs || state.BehindOurs,
		Current: l.ID == selected,
	}
	for _, pair := range l.Pairs {
		ours, console := "", ""
		if p := st.Person(pair.Ours); p != nil {
			ours, console = p.Username, consoleLine(st, *p)
		}
		v.Pairs = append(v.Pairs, pairView{ID: pair.ID, Ours: ours, OursConsole: console, Theirs: pair.TheirName})
	}
	return v
}

func (a *App) linksView(selected string) linksView {
	a.purge()
	st := a.Store.Snapshot()
	v := linksView{Waiting: waitingRows(&st, true)}
	for _, l := range st.Links {
		row := a.linkRow(&st, l, selected)
		v.Servers = append(v.Servers, row)
		if l.ID == selected {
			chosen := row
			v.Selected = &chosen
		}
	}
	return v
}

// gb reads as a whole number once it is big enough for a decimal to mean nothing.
func gb(bytes uint64) string {
	if bytes < 50_000_000 {
		return "0"
	}
	if bytes >= 10_000_000_000 {
		return strconv.FormatUint(bytes/1_000_000_000, 10)
	}
	return strconv.FormatFloat(float64(bytes)/1e9, 'f', 1, 64)
}

func percent(used, limit uint64) int {
	if limit == 0 {
		return 100
	}
	return int(min(100, used*100/limit))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (a *App) relayView() relayView {
	r := a.Relay.Status()
	found := r.Found
	if found == "" {
		found = "Not found"
	}
	var day int
	a.Store.View(func(st *store.State) { day = st.Settings.RelayResetDay })
	used := percent(r.UsedBytes, r.LimitBytes)
	return relayView{
		On: r.On, UsedGB: gb(r.UsedBytes), LimitGB: int(r.LimitBytes / 1_000_000_000), UsedPercent: used,
		Warn: used >= 90, AtLimit: r.AtLimit, Resets: r.Resets.Format("January 2"), ResetDay: day,
		GamesMonth: r.Games, GamesMonthText: plural(r.Games, "game"), Found: found, TestFailed: r.TestFailed,
		GamesAtOnce: r.GamesAtOnce, MaxGames: relay.MaxGames, FirstPort: r.FirstPort, LastPort: r.LastPort,
	}
}

func gamesLine(g store.Games) string {
	played := g.Direct + g.Relayed
	if played == 0 {
		return "0"
	}
	return fmt.Sprintf("%d, %d%% direct", played, g.Direct*100/played)
}

func (a *App) statusView() statusView {
	a.purge()
	st := a.Store.Snapshot()
	r := a.relayView()
	v := statusView{
		RelayOn: r.On, UsedGB: r.UsedGB, LimitGB: r.LimitGB, UsedPercent: r.UsedPercent, Warn: r.Warn, Resets: r.Resets,
		PeopleCount: len(st.People), Pending: len(waitingRows(&st, false)), GamesLine: gamesLine(st.Games),
		LinkCount: len(st.Links), Update: a.Updates.Latest(),
	}
	for _, p := range st.People {
		if a.People.Online(p.ID) {
			v.OnlineCount++
		}
	}
	for _, l := range st.Links {
		state := a.Links.State(l.ID)
		url := "/links?server=" + l.ID
		switch {
		case state.BehindOurs:
			v.Problems = append(v.Problems, problemView{"This server needs an update", url})
		case state.BehindTheirs:
			v.Problems = append(v.Problems, problemView{l.Name + " needs an update", url})
		case !state.Reachable:
			v.Problems = append(v.Problems, problemView{"Can't reach " + l.Name, url})
		}
	}
	if r.On && r.AtLimit {
		v.Problems = append(v.Problems, problemView{"Relay limit reached", "/relay"})
	}
	return v
}

func (a *App) gamesView() gamesView {
	var g store.Games
	a.Store.View(func(st *store.State) { g = st.Games })
	v := gamesView{Games: g.Direct + g.Relayed, DirectCount: g.Direct, RelayedCount: g.Relayed, Failed: g.Failed}
	if v.Games > 0 {
		v.Direct = g.Direct * 100 / v.Games
		v.Relayed = 100 - v.Direct
	}
	return v
}

func (a *App) serverView() serverView {
	st := a.Store.Snapshot()
	v := serverView{Name: st.Settings.Name, Address: st.Settings.Address, LinkCount: len(st.Links), Update: a.Updates.Latest()}
	if v.Update != "" {
		v.UpdateURL = update.PageURL
	}
	return v
}

func (a *App) settingsView() settingsView {
	var current string
	a.Store.View(func(st *store.State) { current = st.Settings.Theme })
	v := settingsView{Version: a.Version}
	for i, t := range themes {
		v.Themes = append(v.Themes, themeView{ID: t.ID, Name: t.Name, Current: t.ID == current || (i == 0 && current == "")})
	}
	return v
}

// purge drops codes past their time. A code is refused at its exact time anyway; this keeps
// the page and the file from holding it.
func (a *App) purge() {
	now := time.Now()
	expired := func(c store.Code) bool { return !c.Live(now) }
	any := false
	a.Store.View(func(st *store.State) { any = slices.ContainsFunc(st.Codes, expired) })
	if any {
		a.Store.Update(func(st *store.State) error {
			st.Codes = slices.DeleteFunc(st.Codes, expired)
			return nil
		})
	}
}

// query reads a query value, which only ever picks something by id.
func query(r *http.Request, name string) string {
	return r.URL.Query().Get(name)
}
