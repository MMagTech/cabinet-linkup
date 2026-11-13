package admin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	codeLife     = 7 * 24 * time.Hour
	maxCodes     = 200
	maxStun      = 16
	maxAddress   = 255
	steps        = "On your console: Settings, Streaming, turn on LinkUp, then Server, Add a server."
	linkSteps    = "On your admin page: Server, Linked servers, Enter code."
	wordRequired = "Required"
	wordTooLong  = "Too long"
	wordInvalid  = "Not valid"
)

func (a *App) done(w http.ResponseWriter, r *http.Request, s *session, where, text string) {
	if text != "" {
		a.sessions.setFlash(s, &Notice{Text: text, Tone: "done"})
	}
	http.Redirect(w, r, where, http.StatusSeeOther)
}

// saved runs a change and logs if the disk refused it.
func (a *App) saved(change func(*store.State) error) error {
	err := a.Store.Update(change)
	if err != nil {
		a.Log.Problem(logfile.ProblemSave)
	}
	return err
}

// plainText says whether free text holds nothing that could break a line: an address or a name
// is only ever shown and handed out, but it is still one line.
func plainText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func checkName(name string) string {
	switch {
	case name == "":
		return wordRequired
	case utf8.RuneCountInString(name) > store.MaxNameLength:
		return wordTooLong
	case !plainText(name):
		return wordInvalid
	}
	return ""
}

func checkAddress(address string) string {
	switch {
	case address == "":
		return wordRequired
	case len(address) > maxAddress:
		return wordTooLong
	case !plainText(address):
		return wordInvalid
	}
	return ""
}

func field(r *http.Request, name string) string {
	return strings.TrimSpace(r.FormValue(name))
}

// ---- Getting in ----

type passwordErrors struct {
	Error, ErrorShort string
}

func (a *App) getFirstPassword(w http.ResponseWriter, r *http.Request) {
	if a.Store.AdminHash() != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !homeName(r.Host) {
		a.bare(w, http.StatusForbidden, part{"setup-name", nil})
		return
	}
	a.bare(w, http.StatusOK, part{"setup-password", passwordErrors{}})
}

// postFirstPassword: whoever opens the page first sets the password, as most self-hosted apps
// do. The port is not forwarded, and on a VPS it is published only to the VPS itself. The page must
// be opened by a name only a home network can give it, so that a web page someone at home is
// reading cannot point its own name at the server and set the password first.
func (a *App) postFirstPassword(w http.ResponseWriter, r *http.Request) {
	a.first.Lock()
	defer a.first.Unlock()
	if a.Store.AdminHash() != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !homeName(r.Host) {
		a.bare(w, http.StatusForbidden, part{"setup-name", nil})
		return
	}
	password := r.FormValue("password")
	switch {
	case len(password) < minPassword:
		a.bare(w, http.StatusOK, part{"setup-password", passwordErrors{ErrorShort: "Too short"}})
		return
	case password != r.FormValue("again"):
		a.bare(w, http.StatusOK, part{"setup-password", passwordErrors{Error: "Doesn't match"}})
		return
	}
	if a.Store.SetAdminHash(hashPassword(password)) != nil {
		a.Log.Problem(logfile.ProblemSave)
		http.Error(w, "The data folder could not be written", http.StatusInternalServerError)
		return
	}
	a.sessions.start(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type signinView struct {
	Error string
}

func (a *App) getSignin(w http.ResponseWriter, r *http.Request) {
	if a.Store.AdminHash() == "" {
		http.Redirect(w, r, "/setup/password", http.StatusSeeOther)
		return
	}
	if a.sessions.get(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.bare(w, http.StatusOK, part{"signin", signinView{}})
}

func (a *App) postSignin(w http.ResponseWriter, r *http.Request) {
	stored := a.Store.AdminHash()
	if stored == "" {
		http.Redirect(w, r, "/setup/password", http.StatusSeeOther)
		return
	}
	giveBack, ok := a.sessions.takeTry()
	if !ok {
		a.bare(w, http.StatusTooManyRequests, part{"signin", signinView{Error: "Too many tries"}})
		return
	}
	if !checkPassword(stored, r.FormValue("password")) {
		a.bare(w, http.StatusOK, part{"signin", signinView{Error: "Incorrect"}})
		return
	}
	giveBack()
	a.sessions.start(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) postSignout(w http.ResponseWriter, r *http.Request, _ *session) {
	a.sessions.end(w, r)
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

type passwordView struct {
	ErrorCurrent, ErrorShort, ErrorAgain, Back string
}

func (a *App) drawPassword(w http.ResponseWriter, s *session, errs passwordView) {
	errs.Back = "/server/settings"
	a.app(w, s, navView{OnServer: true}, part{"server-settings", a.settingsView()}, part{"password", errs})
}

func (a *App) getPassword(w http.ResponseWriter, _ *http.Request, s *session) {
	a.drawPassword(w, s, passwordView{})
}

func (a *App) postPassword(w http.ResponseWriter, r *http.Request, s *session) {
	giveBack, ok := a.sessions.takeTry()
	if !ok {
		a.drawPassword(w, s, passwordView{ErrorCurrent: "Too many tries"})
		return
	}
	if !checkPassword(a.Store.AdminHash(), r.FormValue("current")) {
		a.drawPassword(w, s, passwordView{ErrorCurrent: "Incorrect"})
		return
	}
	giveBack()
	password := r.FormValue("password")
	switch {
	case len(password) < minPassword:
		a.drawPassword(w, s, passwordView{ErrorShort: "Too short"})
		return
	case password != r.FormValue("again"):
		a.drawPassword(w, s, passwordView{ErrorAgain: "Doesn't match"})
		return
	}
	if a.Store.SetAdminHash(hashPassword(password)) != nil {
		a.Log.Problem(logfile.ProblemSave)
		a.drawPassword(w, s, passwordView{ErrorAgain: "Could not save"})
		return
	}
	a.sessions.endOthers(s)
	a.done(w, r, s, "/server/settings", "Saved")
}

// ---- First run ----

type setupAddressView struct {
	Name, Address, ErrorName, ErrorAddress string
}

func (a *App) getSetupAddress(w http.ResponseWriter, r *http.Request, _ *session) {
	st := a.Store.Snapshot()
	if st.Setup.AddressDone {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.bare(w, http.StatusOK, part{"setup-address", setupAddressView{Name: st.Settings.Name, Address: st.Settings.Address}})
}

func (a *App) postSetupAddress(w http.ResponseWriter, r *http.Request, _ *session) {
	v := setupAddressView{Name: field(r, "name"), Address: field(r, "address")}
	v.ErrorName, v.ErrorAddress = checkName(v.Name), checkAddress(v.Address)
	if v.ErrorName != "" || v.ErrorAddress != "" {
		a.bare(w, http.StatusOK, part{"setup-address", v})
		return
	}
	a.saved(func(st *store.State) error {
		st.Settings.Name, st.Settings.Address = v.Name, v.Address
		st.Setup.AddressDone = true
		return nil
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type setupRelayView struct {
	LimitGB               int
	GamesAtOnce, MaxGames int
	FirstPort, LastPort   int
}

func (a *App) getSetupRelay(w http.ResponseWriter, r *http.Request, _ *session) {
	st := a.Store.Snapshot()
	if st.Setup.RelayDone {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	rel := a.Relay.Status()
	a.bare(w, http.StatusOK, part{"setup-relay", setupRelayView{
		LimitGB: st.Settings.RelayLimitGB, GamesAtOnce: rel.GamesAtOnce, MaxGames: relay.MaxGames, FirstPort: rel.FirstPort, LastPort: rel.LastPort,
	}})
}

func (a *App) postSetupRelay(w http.ResponseWriter, r *http.Request, _ *session) {
	limit, err := strconv.Atoi(field(r, "gb"))
	games, errGames := strconv.Atoi(field(r, "games"))
	a.saved(func(st *store.State) error {
		if err == nil && limit >= 1 {
			st.Settings.RelayLimitGB = limit
		}
		if errGames == nil {
			st.Settings.RelayGames = min(max(games, 1), relay.MaxGames)
		}
		st.Settings.Relay = true
		st.Setup.RelayDone = true
		return nil
	})
	a.Relay.Apply()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) getSetupRelaySkip(w http.ResponseWriter, r *http.Request, _ *session) {
	a.saved(func(st *store.State) error { st.Setup.RelayDone = true; return nil })
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type setupOwnView struct {
	HomeAddress, ErrorAddress string
}

func (a *App) getSetupOwn(w http.ResponseWriter, _ *http.Request, _ *session) {
	var local string
	a.Store.View(func(st *store.State) { local = st.Settings.LocalAddress })
	a.bare(w, http.StatusOK, part{"setup-own", setupOwnView{HomeAddress: local}})
}

func (a *App) postSetupOwn(w http.ResponseWriter, r *http.Request, _ *session) {
	v := setupOwnView{HomeAddress: field(r, "address")}
	if v.ErrorAddress = checkAddress(v.HomeAddress); v.ErrorAddress != "" {
		a.bare(w, http.StatusOK, part{"setup-own", v})
		return
	}
	code := newCode(store.Code{Kind: store.CodeOwn, Note: "Your own console"})
	err := a.saved(func(st *store.State) error {
		st.Codes = slices.DeleteFunc(st.Codes, func(c store.Code) bool { return c.Kind == store.CodeOwn })
		st.Codes = append(st.Codes, code)
		st.Settings.LocalAddress = v.HomeAddress
		st.Setup.OwnDone = true
		return nil
	})
	if err != nil {
		v.ErrorAddress = "Could not save"
		a.bare(w, http.StatusOK, part{"setup-own", v})
		return
	}
	http.Redirect(w, r, "/setup/own/code?id="+code.ID, http.StatusSeeOther)
}

func (a *App) getSetupOwnSkip(w http.ResponseWriter, r *http.Request, _ *session) {
	a.saved(func(st *store.State) error { st.Setup.OwnDone = true; return nil })
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

type ownCodeView struct {
	Address, Code, Expires string
}

func (a *App) getSetupOwnCode(w http.ResponseWriter, r *http.Request, _ *session) {
	st := a.Store.Snapshot()
	c := st.Code(query(r, "id"))
	if c == nil {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	a.bare(w, http.StatusOK, part{"setup-own-code", ownCodeView{Address: st.Settings.LocalAddress, Code: c.Code, Expires: rfc(c.Expires)}})
}

// newCode makes a code and its slow hash, with its id and its time filled in. A person's own code
// has no time: it lasts until it is changed.
func newCode(c store.Code) store.Code {
	c.ID = store.NewID()
	c.Code = proof.NewCode()
	tidy, _ := proof.TidyCode(c.Code)
	hashing.Lock()
	c.K = proof.SlowHash(tidy)
	hashing.Unlock()
	if c.Kind != store.CodePerson {
		c.Expires = time.Now().Add(codeLife)
	}
	return c
}

// ---- Status ----

func (a *App) getStatus(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnStatus: true}, part{"status-page", a.statusView()})
}

func (a *App) getGames(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnStatus: true}, part{"games-page", a.gamesView()})
}

// ---- People ----

func (a *App) getPeople(w http.ResponseWriter, r *http.Request, s *session) {
	a.app(w, s, navView{OnPeople: true}, part{"people", a.peopleView(query(r, "person"))})
}

type peopleAddView struct {
	Note, Back string
	Accounts   []option
}

func (a *App) getPeopleAdd(w http.ResponseWriter, _ *http.Request, s *session) {
	st := a.Store.Snapshot()
	a.app(w, s, navView{OnPeople: true}, part{"people", a.peopleView("")},
		part{"people-add", peopleAddView{Back: "/people", Accounts: accountOptions(&st, joinsOnly)}})
}

func (a *App) addCode(w http.ResponseWriter, r *http.Request, s *session, c store.Code, back string) (store.Code, bool) {
	c = newCode(c)
	err := a.saved(func(st *store.State) error {
		if len(st.Codes) >= maxCodes {
			return errors.New("too many codes")
		}
		st.Codes = append(st.Codes, c)
		return nil
	})
	if err != nil {
		a.sessions.setFlash(s, &Notice{Text: "Too many codes waiting", Tone: "problem"})
		http.Redirect(w, r, back, http.StatusSeeOther)
		return c, false
	}
	return c, true
}

func (a *App) postPeopleAdd(w http.ResponseWriter, r *http.Request, s *session) {
	note := noteText(r)
	forID := r.FormValue("for")
	st := a.Store.Snapshot()
	// An invite is someone's who can host, while there is anyone who can: a friend who only joins
	// could never play with whoever uses it from 2P Start.
	if f := st.Person(forID); f == nil || f.JoinsOnly {
		if len(accountOptions(&st, joinsOnly)) > 0 {
			http.Redirect(w, r, "/people/add", http.StatusSeeOther)
			return
		}
		forID = ""
	}
	c, ok := a.addCode(w, r, s, store.Code{Kind: store.CodeInvite, Note: note, For: forID}, "/people")
	if ok {
		http.Redirect(w, r, "/people/code?id="+c.ID, http.StatusSeeOther)
	}
}

// noteText cuts a note to its limit. The field holds it there already, so this only matters for
// what did not come through the field.
func noteText(r *http.Request) string {
	note := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, field(r, "note"))
	if utf8.RuneCountInString(note) > store.MaxNoteLength {
		note = string([]rune(note)[:store.MaxNoteLength])
	}
	return note
}

func (a *App) getPeopleCode(w http.ResponseWriter, r *http.Request, s *session) {
	st := a.Store.Snapshot()
	c := st.Code(query(r, "id"))
	if c == nil || c.Kind == store.CodeLink {
		http.Redirect(w, r, "/people", http.StatusSeeOther)
		return
	}
	view := readyView{
		Steps: steps, Address: st.Settings.Address, Code: c.Code, Expires: rfc(c.Expires), ID: c.ID,
		CancelURL: "/people/codes/cancel", Back: "/people",
	}
	selected := ""
	switch c.Kind {
	case store.CodeOwn:
		view.Title, view.CancelWord, view.Address = "Your console", "Cancel code", st.Settings.LocalAddress
		view.Intro = "The code for my own console."
	case store.CodePerson:
		p := st.Person(c.For)
		if p == nil {
			http.Redirect(w, r, "/people", http.StatusSeeOther)
			return
		}
		view.Title, view.Expires, view.CancelURL = "Code for "+nameOf(&st, *p), "", ""
		view.Intro = "Here's your LinkUp code. It works on each of your consoles, signed in as you."
		view.ChangeURL = "/people/change-code?person=" + p.ID
		selected, view.Back = p.ID, "/people?person="+p.ID
	default:
		view.Title, view.CancelWord, view.Who = "Invite ready", "Cancel invite", c.Note
		view.Intro = "Let's play on CabinetOS with LinkUp."
	}
	a.app(w, s, navView{OnPeople: true}, part{"people", a.peopleView(selected)}, part{"ready", view})
}

func (a *App) postPeopleCodeCancel(w http.ResponseWriter, r *http.Request, s *session) {
	a.cancelCode(r)
	a.done(w, r, s, "/people", "Invite cancelled")
}

func (a *App) cancelCode(r *http.Request) {
	a.saved(func(st *store.State) error { st.RemoveCode(r.FormValue("code")); return nil })
}

func (a *App) postPeopleNote(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.FormValue("person")
	note := noteText(r)
	a.saved(func(st *store.State) error {
		if p := st.Person(id); p != nil {
			p.Note = note
		}
		return nil
	})
	a.done(w, r, s, "/people?person="+id, "Saved")
}

// postPersonCode shows someone's own code: the invite they used, or a new one for someone who came
// in before codes worked this way. One code for each of their consoles, until it is changed.
func (a *App) postPersonCode(w http.ResponseWriter, r *http.Request, s *session) {
	person := r.FormValue("person")
	st := a.Store.Snapshot()
	if st.Person(person) == nil {
		http.Redirect(w, r, "/people", http.StatusSeeOther)
		return
	}
	if c := st.CodeOf(person); c != nil {
		http.Redirect(w, r, "/people/code?id="+c.ID, http.StatusSeeOther)
		return
	}
	c, ok := a.addCode(w, r, s, store.Code{Kind: store.CodePerson, For: person}, "/people?person="+person)
	if ok {
		http.Redirect(w, r, "/people/code?id="+c.ID, http.StatusSeeOther)
	}
}

func (a *App) getPersonConfirm(w http.ResponseWriter, r *http.Request, s *session, action, yes string) {
	st := a.Store.Snapshot()
	p := st.Person(query(r, "person"))
	if p == nil {
		http.Redirect(w, r, "/people", http.StatusSeeOther)
		return
	}
	name := nameOf(&st, *p)
	v := confirmView{Action: action, ID: p.ID, Yes: yes, Back: "/people?person=" + p.ID}
	if action == "/people/change-code" {
		v.Question = "Change " + name + "'s code?"
		v.Sub = "Every console " + name + " is on is signed out until the new code is typed on it."
	} else {
		v.Question = "Remove " + name + "?"
		v.Sub = name + "'s friends go too, and every console " + name + " is on is signed out."
	}
	a.app(w, s, navView{OnPeople: true}, part{"people", a.peopleView(p.ID)}, part{"confirm", v})
}

func (a *App) getChangeCode(w http.ResponseWriter, r *http.Request, s *session) {
	a.getPersonConfirm(w, r, s, "/people/change-code", "Change code")
}

func (a *App) getPersonRemove(w http.ResponseWriter, r *http.Request, s *session) {
	a.getPersonConfirm(w, r, s, "/people/remove", "Remove")
}

// signOut takes someone off every console they are on, and tells those consoles and whoever can see
// them. With remove, they are gone too, with their friends.
func (a *App) signOut(id string, remove bool) (gone []int, ok bool) {
	var touched []string
	a.saved(func(st *store.State) error {
		if st.Person(id) == nil {
			return nil
		}
		ok = true
		touched = append(st.FriendsOf(id), id)
		gone = st.SignOut(id)
		if remove {
			st.RemovePerson(id)
		}
		return nil
	})
	for _, n := range gone {
		a.Log.ConsoleRemoved(n)
		a.People.ConsoleRemoved(n)
	}
	a.People.FriendsChanged(touched...)
	a.Links.Changed()
	return gone, ok
}

// postChangeCode gives someone a new code and signs them out of every console, which they then add
// again with it. They keep their friends.
func (a *App) postChangeCode(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.FormValue("id")
	if _, ok := a.signOut(id, false); !ok {
		http.Redirect(w, r, "/people", http.StatusSeeOther)
		return
	}
	a.saved(func(st *store.State) error {
		st.Codes = slices.DeleteFunc(st.Codes, func(c store.Code) bool { return c.Kind == store.CodePerson && c.For == id })
		return nil
	})
	c, ok := a.addCode(w, r, s, store.Code{Kind: store.CodePerson, For: id}, "/people?person="+id)
	if ok {
		a.done(w, r, s, "/people/code?id="+c.ID, "Code changed")
	}
}

func (a *App) postPersonRemove(w http.ResponseWriter, r *http.Request, s *session) {
	a.signOut(r.FormValue("id"), true)
	a.done(w, r, s, "/people", "Removed")
}

func (a *App) postFriendAdd(w http.ResponseWriter, r *http.Request, s *session) {
	person, friend := r.FormValue("person"), r.FormValue("friend")
	added := false
	a.saved(func(st *store.State) error {
		if p, f := st.Person(person), st.Person(friend); p != nil && f != nil && person != friend && store.CanPlay(p, f) {
			st.AddFriends(person, friend)
			added = true
		}
		return nil
	})
	if !added {
		http.Redirect(w, r, "/people?person="+person, http.StatusSeeOther)
		return
	}
	a.People.FriendsChanged(person, friend)
	a.done(w, r, s, "/people?person="+person, "Friend added")
}

func (a *App) postFriendRemove(w http.ResponseWriter, r *http.Request, s *session) {
	person, id := r.FormValue("person"), r.FormValue("id")
	switch {
	case strings.HasPrefix(id, "f:"):
		other := strings.TrimPrefix(id, "f:")
		a.saved(func(st *store.State) error { st.RemoveFriends(person, other); return nil })
		a.People.EndBetween(person, other)
		a.People.FriendsChanged(person, other)
	case strings.HasPrefix(id, "p:"):
		if link, pair, ok := strings.Cut(strings.TrimPrefix(id, "p:"), ":"); ok {
			a.Links.RemovePair(link, pair)
		}
	}
	a.done(w, r, s, "/people?person="+person, "Removed")
}

// ---- Relay ----

func (a *App) getRelay(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnRelay: true}, part{"relay", a.relayView()})
}

func (a *App) postRelaySwitch(on bool) page {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		a.saved(func(st *store.State) error { st.Settings.Relay = on; return nil })
		a.Relay.Apply()
		a.done(w, r, s, "/relay", "")
	}
}

func (a *App) postRelayTest(w http.ResponseWriter, r *http.Request, s *session) {
	took, err := a.Relay.Test()
	if err != nil {
		a.done(w, r, s, "/relay", "")
		return
	}
	a.done(w, r, s, "/relay", fmt.Sprintf("Test passed, %d ms", took.Milliseconds()))
}

func (a *App) postRelayLimit(w http.ResponseWriter, r *http.Request, s *session) {
	limit, errLimit := strconv.Atoi(field(r, "gb"))
	day, errDay := strconv.Atoi(field(r, "day"))
	games, errGames := strconv.Atoi(field(r, "games"))
	a.saved(func(st *store.State) error {
		if errLimit == nil && limit >= 1 {
			st.Settings.RelayLimitGB = limit
		}
		if errDay == nil {
			st.Settings.RelayResetDay = min(max(day, 1), 31)
		}
		if errGames == nil {
			st.Settings.RelayGames = min(max(games, 1), relay.MaxGames)
		}
		return nil
	})
	a.Relay.Apply()
	a.done(w, r, s, "/relay", "Saved")
}

// ---- Server ----

func (a *App) getServer(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnServer: true}, part{"server", a.serverView()})
}

func (a *App) postServerAbout(w http.ResponseWriter, r *http.Request, s *session) {
	name, address := field(r, "name"), field(r, "address")
	errName, errAddress := checkName(name), checkAddress(address)
	if errName != "" || errAddress != "" {
		v := a.serverView()
		v.Name, v.Address, v.ErrorName, v.ErrorAddress = name, address, errName, errAddress
		a.app(w, s, navView{OnServer: true}, part{"server", v})
		return
	}
	a.saved(func(st *store.State) error {
		st.Settings.Name, st.Settings.Address = name, address
		return nil
	})
	a.Relay.Apply()
	a.Links.Changed()
	a.done(w, r, s, "/server", "Saved")
}

func (a *App) getSettings(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnServer: true}, part{"server-settings", a.settingsView()})
}

func (a *App) postTheme(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.FormValue("theme")
	if slices.ContainsFunc(themes, func(t theme) bool { return t.ID == id }) {
		a.saved(func(st *store.State) error { st.Settings.Theme = id; return nil })
	}
	a.done(w, r, s, "/server/settings", "")
}

type stunPageView struct {
	Stun []stunView
}

func (a *App) getStun(w http.ResponseWriter, _ *http.Request, s *session) {
	var v stunPageView
	a.Store.View(func(st *store.State) {
		for _, x := range st.Settings.Stun {
			v.Stun = append(v.Stun, stunView{ID: x.ID, Address: x.Address})
		}
	})
	a.app(w, s, navView{OnServer: true}, part{"server-stun", v})
}

func (a *App) postStun(w http.ResponseWriter, r *http.Request, s *session) {
	id, address := r.FormValue("id"), field(r, "address")
	word := ""
	a.saved(func(st *store.State) error {
		list := st.Settings.Stun
		i := slices.IndexFunc(list, func(x store.Stun) bool { return x.ID == id })
		switch r.PathValue("what") {
		case "up":
			if i > 0 {
				list[i-1], list[i] = list[i], list[i-1]
			}
		case "down":
			if i >= 0 && i < len(list)-1 {
				list[i+1], list[i] = list[i], list[i+1]
			}
		case "remove":
			if i >= 0 {
				st.Settings.Stun = slices.Delete(list, i, i+1)
			}
		case "add":
			if checkAddress(address) == "" && len(list) < maxStun {
				st.Settings.Stun = append(list, store.Stun{ID: store.NewID(), Address: address})
			}
		case "reset":
			st.Settings.Stun = nil
			for _, x := range store.DefaultStun {
				st.Settings.Stun = append(st.Settings.Stun, store.Stun{ID: store.NewID(), Address: x})
			}
			word = "Reset"
		}
		return nil
	})
	a.done(w, r, s, "/server/stun", word)
}

type logView struct {
	Log []logfile.Line
}

func (a *App) getLog(w http.ResponseWriter, _ *http.Request, s *session) {
	a.app(w, s, navView{OnServer: true}, part{"server-log", logView{Log: a.Log.Tail(200)}})
}

func (a *App) getLogFile(w http.ResponseWriter, _ *http.Request, _ *session) {
	data, err := os.ReadFile(a.Log.Path())
	if err != nil {
		http.Error(w, "No log", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="linkup.log"`)
	w.Write(data)
}

// ---- Linked servers ----

func (a *App) getLinks(w http.ResponseWriter, r *http.Request, s *session) {
	a.app(w, s, navView{OnLinks: true}, part{"links", a.linksView(query(r, "server"))})
}

type linkMakeView struct {
	Back     string
	Accounts []option
}

func (a *App) getLinkMake(w http.ResponseWriter, _ *http.Request, s *session) {
	st := a.Store.Snapshot()
	a.app(w, s, navView{OnLinks: true}, part{"links", a.linksView("")},
		part{"links-make", linkMakeView{Back: "/links", Accounts: accountOptions(&st, nil)}})
}

func (a *App) postLinkMake(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.FormValue("for")
	st := a.Store.Snapshot()
	p := st.Person(id)
	if p == nil {
		http.Redirect(w, r, "/links/make", http.StatusSeeOther)
		return
	}
	c, ok := a.addCode(w, r, s, store.Code{Kind: store.CodeLink, Note: "For " + nameOf(&st, *p), For: id}, "/links")
	if ok {
		http.Redirect(w, r, "/links/code?id="+c.ID, http.StatusSeeOther)
	}
}

func (a *App) getLinkCode(w http.ResponseWriter, r *http.Request, s *session) {
	st := a.Store.Snapshot()
	c := st.Code(query(r, "id"))
	if c == nil || c.Kind != store.CodeLink {
		http.Redirect(w, r, "/links", http.StatusSeeOther)
		return
	}
	name := ""
	if p := st.Person(c.For); p != nil {
		name = nameOf(&st, *p)
	}
	a.app(w, s, navView{OnLinks: true}, part{"links", a.linksView("")}, part{"ready", readyView{
		Title: "Link code", Who: "For " + name, Intro: "Let's link our servers. This code is for " + name + ".", Steps: linkSteps,
		Address: st.Settings.Address, Code: c.Code, Expires: rfc(c.Expires), ID: c.ID,
		CancelURL: "/links/codes/cancel", CancelWord: "Cancel code", Back: "/links",
	}})
}

func (a *App) postLinkCodeCancel(w http.ResponseWriter, r *http.Request, s *session) {
	a.cancelCode(r)
	a.done(w, r, s, "/links", "Code cancelled")
}

type linkEnterView struct {
	Back, Address, Code, Error, ErrorAddress, ErrorAccount string
	Accounts                                               []option
}

func (a *App) drawLinkEnter(w http.ResponseWriter, s *session, v linkEnterView) {
	st := a.Store.Snapshot()
	v.Back, v.Accounts = "/links", accountOptions(&st, nil)
	a.app(w, s, navView{OnLinks: true}, part{"links", a.linksView("")}, part{"links-enter", v})
}

func (a *App) getLinkEnter(w http.ResponseWriter, _ *http.Request, s *session) {
	a.drawLinkEnter(w, s, linkEnterView{})
}

func (a *App) postLinkEnter(w http.ResponseWriter, r *http.Request, s *session) {
	v := linkEnterView{Address: field(r, "address"), Code: field(r, "code")}
	v.ErrorAddress = checkAddress(v.Address)
	if v.Code == "" {
		v.Error = wordRequired
	}
	person := r.FormValue("for")
	if st := a.Store.Snapshot(); st.Person(person) == nil {
		v.ErrorAccount = wordRequired
	}
	if v.Error != "" || v.ErrorAddress != "" || v.ErrorAccount != "" {
		a.drawLinkEnter(w, s, v)
		return
	}
	switch err := a.Links.Enter(v.Address, v.Code, person); {
	case err == nil:
		a.done(w, r, s, "/links", "Linked")
	case errors.Is(err, store.ErrCantReach):
		v.ErrorAddress = "Can't reach"
		a.drawLinkEnter(w, s, v)
	default:
		v.Error = "Wrong or expired"
		a.drawLinkEnter(w, s, v)
	}
}

func (a *App) getLinkRemove(w http.ResponseWriter, r *http.Request, s *session) {
	st := a.Store.Snapshot()
	id := query(r, "id")
	l := st.Link(id)
	if l == nil {
		http.Redirect(w, r, "/links", http.StatusSeeOther)
		return
	}
	a.app(w, s, navView{OnLinks: true}, part{"links", a.linksView(id)},
		part{"confirm", confirmView{Question: "Unlink " + l.Name + "?", Action: "/links/remove", ID: id, Yes: "Unlink", Back: "/links?server=" + id}})
}

func (a *App) postLinkRemove(w http.ResponseWriter, r *http.Request, s *session) {
	a.Links.Unlink(r.FormValue("id"))
	a.done(w, r, s, "/links", "Unlinked")
}

func (a *App) postPairRemove(w http.ResponseWriter, r *http.Request, s *session) {
	pair := r.FormValue("pair")
	st := a.Store.Snapshot()
	for _, l := range st.Links {
		if slices.ContainsFunc(l.Pairs, func(p store.Pair) bool { return p.ID == pair }) {
			a.Links.RemovePair(l.ID, pair)
			a.done(w, r, s, "/links?server="+l.ID, "Removed")
			return
		}
	}
	http.Redirect(w, r, "/links", http.StatusSeeOther)
}
