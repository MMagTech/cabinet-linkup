package admin

import (
	"bytes"
	"io"
	"net/http"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

// Notice is a toast: something that went fine. What needs fixing goes at its field instead.
type Notice struct {
	Text string
	Tone string
}

// theme is one of the looks under Settings. CabinetOS is admin.css's own colours, and a theme
// with a file goes on top of it.
type theme struct {
	ID, Name, File, Color string
}

var themes = []theme{
	{"cabinet", "CabinetOS", "", "#1a1430"},
	{"graphite", "Graphite", "graphite", "#0f1012"},
	{"midnight", "Midnight", "midnight", "#0c1428"},
	{"light", "Light", "light", "#f3f5f8"},
}

// part is one template and what it draws.
type part struct {
	name string
	data any
}

type headView struct {
	Theme, ThemeColor, V string
}

type footView struct {
	Notice *Notice
	V      string
}

type navView struct {
	OnStatus, OnPeople, OnRelay, OnServer, OnLinks bool
}

func (a *App) head() headView {
	chosen := themes[0]
	a.Store.View(func(st *store.State) {
		for _, t := range themes {
			if t.ID == st.Settings.Theme {
				chosen = t
			}
		}
	})
	return headView{Theme: chosen.File, ThemeColor: chosen.Color, V: a.Version}
}

// write draws the page into memory first, so a template that fails never leaves half a page.
func (a *App) write(w http.ResponseWriter, status int, notice *Notice, draw func(*bytes.Buffer) error) {
	var b bytes.Buffer
	err := a.tmpl.ExecuteTemplate(&b, "head", a.head())
	if err == nil {
		err = draw(&b)
	}
	if err == nil {
		err = a.tmpl.ExecuteTemplate(&b, "foot", footView{Notice: notice, V: a.Version})
	}
	if err != nil {
		a.Log.Problem(logfile.ProblemInternal)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.Copy(w, &b)
}

func (a *App) parts(b *bytes.Buffer, parts []part) error {
	for _, p := range parts {
		if err := a.tmpl.ExecuteTemplate(b, p.name, p.data); err != nil {
			return err
		}
	}
	return nil
}

// bare draws a screen with no navigation: first run, sign-in, refused.
func (a *App) bare(w http.ResponseWriter, status int, parts ...part) {
	a.write(w, status, nil, func(b *bytes.Buffer) error { return a.parts(b, parts) })
}

// app draws a screen inside the navigation. A sheet is drawn over the page it opened from: the
// page's part, then the sheet's.
func (a *App) app(w http.ResponseWriter, s *session, nav navView, parts ...part) {
	a.write(w, http.StatusOK, a.sessions.takeFlash(s), func(b *bytes.Buffer) error {
		b.WriteString(`<div class="app">`)
		if err := a.tmpl.ExecuteTemplate(b, "nav", nav); err != nil {
			return err
		}
		b.WriteString(`<main>`)
		if err := a.parts(b, parts); err != nil {
			return err
		}
		b.WriteString(`</main></div>`)
		return nil
	})
}
