// Package admin is the admin page: its own port, never forwarded, with html/template and no
// JavaScript framework, laid out for phones and computers from one piece of markup. Every
// form is a plain POST that redirects, so a page needs nothing but HTML to work.
//
// This file holds what stands between a request and a screen: who may reach the page at all,
// and first run. views.go builds what each template draws, forms.go does what each form
// asks, and render.go writes the page.
package admin

import (
	"embed"
	"html/template"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

func init() {
	// A container has no mime.types file, and a nosniff page must be told what a font is.
	mime.AddExtensionType(".woff2", "font/woff2")
}

// People is what the page needs from who is connected now.
type People interface {
	Online(person string) bool
	// FriendsChanged tells these people's consoles that their lists changed.
	FriendsChanged(people ...string)
	// ConsoleRemoved tells a console that it was taken off the server.
	ConsoleRemoved(number int)
}

// LinkState is how a link is doing right now.
type LinkState struct {
	Reachable    bool
	BehindTheirs bool
	BehindOurs   bool
}

// Links is what the page needs from the links to other servers.
type Links interface {
	// Enter uses a code from another server, for one of this server's people. It returns
	// store.ErrWrongCode or store.ErrCantReach.
	Enter(address, code, person string) error
	RemovePair(link, pair string) error
	Unlink(link string) error
	// Changed says this server's name, address or people changed, so linked servers hear now.
	Changed()
	State(link string) LinkState
}

// Relay is what the page needs from the relay.
type Relay interface {
	Apply()
	Status() relay.Status
	Test() (time.Duration, error)
}

// Updates says whether a newer server exists.
type Updates interface {
	Latest() string
}

// Deps is everything the page works with.
type Deps struct {
	Store   *store.Store
	Log     *logfile.Log
	Version string
	People  People
	Links   Links
	Relay   Relay
	Updates Updates
}

// App is the page.
type App struct {
	Deps
	tmpl     *template.Template
	static   fs.FS
	sessions *sessions
	first    sync.Mutex // whoever sets the first password, one at a time
}

// New parses the templates. A template Go refuses stops the server here rather than on a page.
func New(d Deps) (*App, error) {
	t, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return &App{Deps: d, tmpl: t, static: sub, sessions: newSessions()}, nil
}

// Handler is the whole page, from the check of who is asking down to the screens.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes(mux)
	protect := http.NewCrossOriginProtection()
	safe := protect.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.headers(w, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			mux.ServeHTTP(w, r)
			return
		}
		if !FromHome(r) {
			a.bare(w, http.StatusForbidden, part{"refused", nil})
			return
		}
		safe.ServeHTTP(w, r)
	})
}

// headers says what a browser may do with the page: nothing from anywhere else, and only the
// one script of its own. Style attributes stay, for the meters. The referrer stays within the
// site, because a plain-HTTP POST with none sends an Origin of null, which the cross-origin
// check would have to refuse.
func (a *App) headers(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; font-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	if !strings.HasPrefix(r.URL.Path, "/static/") {
		h.Set("Cache-Control", "no-store")
	}
}

var carrierGrade = netip.MustParsePrefix("100.64.0.0/10")

// homeAddress says whether an address counts as not on the public internet. 100.64.0.0/10 counts,
// or an owner's own Tailscale could not open the page.
func homeAddress(text string) bool {
	addr, err := netip.ParseAddr(strings.Trim(text, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || carrierGrade.Contains(addr)
}

// FromHome is the second lock on the page, after the port not being forwarded. It is not proof,
// because some Docker setups hide the visitor's real address. A forwarding header cannot open
// anything: it can only refuse, so a reverse proxy that puts this page on the internet is refused
// even though the proxy itself is at home.
func FromHome(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !homeAddress(host) {
		return false
	}
	for _, list := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(list, ",") {
			if !homeAddress(strings.TrimSpace(hop)) {
				return false
			}
		}
	}
	if real := r.Header.Get("X-Real-Ip"); real != "" && !homeAddress(strings.TrimSpace(real)) {
		return false
	}
	for _, list := range r.Header.Values("Forwarded") {
		for _, element := range strings.Split(list, ",") {
			for _, pair := range strings.Split(element, ";") {
				key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if ok && strings.EqualFold(key, "for") {
					v := strings.Trim(value, `"`)
					if h, _, err := net.SplitHostPort(v); err == nil {
						v = h
					}
					if !homeAddress(v) {
						return false
					}
				}
			}
		}
	}
	return true
}

// nextStep is where first run has got to, or "" when it is finished.
func (a *App) nextStep() string {
	if a.Store.AdminHash() == "" {
		return "/setup/password"
	}
	step := ""
	a.Store.View(func(st *store.State) {
		switch {
		case !st.Setup.AddressDone:
			step = "/setup/address"
		case !st.Setup.RelayDone:
			step = "/setup/relay"
		case !st.Setup.OwnDone:
			step = "/setup/own"
		}
	})
	return step
}

// page is a screen for someone signed in.
type page func(w http.ResponseWriter, r *http.Request, s *session)

// signedIn wraps a screen: no password yet goes to first run, not signed in goes to sign-in.
func (a *App) signedIn(next page) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Store.AdminHash() == "" {
			http.Redirect(w, r, "/setup/password", http.StatusSeeOther)
			return
		}
		s := a.sessions.get(r)
		if s == nil {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		next(w, r, s)
	}
}

// inside wraps a screen that belongs after first run: someone who walked away resumes at the
// first unfinished step.
func (a *App) inside(next page) http.HandlerFunc {
	return a.signedIn(func(w http.ResponseWriter, r *http.Request, s *session) {
		if step := a.nextStep(); step != "" {
			http.Redirect(w, r, step, http.StatusSeeOther)
			return
		}
		next(w, r, s)
	})
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /static/", a.getStatic)
	mux.HandleFunc("GET /{$}", a.signedIn(func(w http.ResponseWriter, r *http.Request, _ *session) {
		step := a.nextStep()
		if step == "" {
			step = "/status"
		}
		http.Redirect(w, r, step, http.StatusSeeOther)
	}))

	mux.HandleFunc("GET /setup/password", a.getFirstPassword)
	mux.HandleFunc("POST /setup/password", a.postFirstPassword)
	mux.HandleFunc("GET /signin", a.getSignin)
	mux.HandleFunc("POST /signin", a.postSignin)

	mux.HandleFunc("GET /setup/address", a.signedIn(a.getSetupAddress))
	mux.HandleFunc("POST /setup/address", a.signedIn(a.postSetupAddress))
	mux.HandleFunc("GET /setup/relay", a.signedIn(a.getSetupRelay))
	mux.HandleFunc("POST /setup/relay", a.signedIn(a.postSetupRelay))
	mux.HandleFunc("GET /setup/relay/skip", a.signedIn(a.getSetupRelaySkip))
	mux.HandleFunc("GET /setup/own", a.signedIn(a.getSetupOwn))
	mux.HandleFunc("POST /setup/own", a.signedIn(a.postSetupOwn))
	mux.HandleFunc("GET /setup/own/skip", a.signedIn(a.getSetupOwnSkip))
	mux.HandleFunc("GET /setup/own/code", a.signedIn(a.getSetupOwnCode))

	mux.HandleFunc("GET /status", a.inside(a.getStatus))
	mux.HandleFunc("GET /status/games", a.inside(a.getGames))

	mux.HandleFunc("GET /people", a.inside(a.getPeople))
	mux.HandleFunc("GET /people/add", a.inside(a.getPeopleAdd))
	mux.HandleFunc("POST /people/add", a.inside(a.postPeopleAdd))
	mux.HandleFunc("GET /people/code", a.inside(a.getPeopleCode))
	mux.HandleFunc("POST /people/codes/cancel", a.inside(a.postPeopleCodeCancel))
	mux.HandleFunc("POST /people/note", a.inside(a.postPeopleNote))
	mux.HandleFunc("POST /consoles/new-code", a.inside(a.postConsoleNewCode))
	mux.HandleFunc("GET /consoles/remove", a.inside(a.getConsoleRemove))
	mux.HandleFunc("POST /consoles/remove", a.inside(a.postConsoleRemove))
	mux.HandleFunc("POST /friends/add", a.inside(a.postFriendAdd))
	mux.HandleFunc("POST /friends/remove", a.inside(a.postFriendRemove))

	mux.HandleFunc("GET /relay", a.inside(a.getRelay))
	mux.HandleFunc("POST /relay/on", a.inside(a.postRelaySwitch(true)))
	mux.HandleFunc("POST /relay/off", a.inside(a.postRelaySwitch(false)))
	mux.HandleFunc("POST /relay/test", a.inside(a.postRelayTest))
	mux.HandleFunc("POST /relay/limit", a.inside(a.postRelayLimit))

	mux.HandleFunc("GET /server", a.inside(a.getServer))
	mux.HandleFunc("POST /server/about", a.inside(a.postServerAbout))
	mux.HandleFunc("GET /server/settings", a.inside(a.getSettings))
	mux.HandleFunc("POST /server/theme", a.inside(a.postTheme))
	mux.HandleFunc("GET /server/stun", a.inside(a.getStun))
	mux.HandleFunc("POST /stun/{what}", a.inside(a.postStun))
	mux.HandleFunc("GET /server/log", a.inside(a.getLog))
	mux.HandleFunc("GET /log", a.inside(a.getLogFile))
	mux.HandleFunc("GET /password", a.inside(a.getPassword))
	mux.HandleFunc("POST /password", a.inside(a.postPassword))
	mux.HandleFunc("POST /signout", a.signedIn(a.postSignout))

	mux.HandleFunc("GET /links", a.inside(a.getLinks))
	mux.HandleFunc("GET /links/make", a.inside(a.getLinkMake))
	mux.HandleFunc("POST /links/make", a.inside(a.postLinkMake))
	mux.HandleFunc("GET /links/code", a.inside(a.getLinkCode))
	mux.HandleFunc("POST /links/codes/cancel", a.inside(a.postLinkCodeCancel))
	mux.HandleFunc("GET /links/enter", a.inside(a.getLinkEnter))
	mux.HandleFunc("POST /links/enter", a.inside(a.postLinkEnter))
	mux.HandleFunc("GET /links/remove", a.inside(a.getLinkRemove))
	mux.HandleFunc("POST /links/remove", a.inside(a.postLinkRemove))
	mux.HandleFunc("POST /links/pairs/remove", a.inside(a.postPairRemove))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusSeeOther) })
}

// getStatic serves the page's own files with a long cache time: every page here is a full page
// load, and a font fetched again on each one shifts the text. The page's own links carry the
// version, so an update is not held back by it.
func (a *App) getStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFileFS(w, r, a.static, name)
}
