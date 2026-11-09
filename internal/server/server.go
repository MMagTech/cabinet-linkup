// Package server puts the parts together: the data folder, the log, the relay, the people, the links,
// and the two ports. It is apart from main so that the tests can run real servers side by side.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/admin"
	"github.com/MMagTech/cabinet-linkup/internal/console"
	"github.com/MMagTech/cabinet-linkup/internal/link"
	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/store"
	"github.com/MMagTech/cabinet-linkup/internal/update"
)

// Config is what a server starts with.
type Config struct {
	Data         string
	ConsoleAddr  string
	AdminAddr    string
	RelayPort    int  // the games' ports follow it
	RelayPrivate bool // testing only: consoles on one home network
	Version      string
	Out          io.Writer
}

// Server is one running server.
type Server struct {
	Store   *store.Store
	Log     *logfile.Log
	Hub     *console.Hub
	Links   *link.Manager
	Relay   *relay.Service
	Admin   *admin.App
	Console *console.Handler
	Updates *update.Checker
	Key     proof.KeyPair

	version string
	console net.Listener
	admin   net.Listener
	servers []*http.Server
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func firstStart(st *store.State) error {
	key, err := proof.NewKeyPair()
	if err != nil {
		return err
	}
	st.ServerKey, st.ServerPub = key.Private, key.Public
	st.RelaySecret = make([]byte, 32)
	_, err = rand.Read(st.RelaySecret)
	return err
}

// New opens the data folder and puts the parts together. Nothing listens until Start.
func New(cfg Config) (*Server, error) {
	st, err := store.Open(cfg.Data, firstStart)
	if err != nil {
		return nil, err
	}
	log, err := logfile.Open(filepath.Join(cfg.Data, "log.txt"), cfg.Out)
	if err != nil {
		return nil, fmt.Errorf("the log cannot be opened: %w", err)
	}
	var key proof.KeyPair
	st.View(func(s *store.State) { key = proof.KeyPair{Private: s.ServerKey, Public: s.ServerPub} })

	rel := relay.New(relay.Config{Port: cfg.RelayPort, AllowPrivate: cfg.RelayPrivate}, st, log)
	hub := console.NewHub(st, log, rel)
	links := link.New(st, log, hub, key)
	hub.SetLinker(links)
	handler := console.NewHandler(hub, st, log, key, links)
	links.OnRemoved = handler.DropLink
	updates := update.New(cfg.Version)
	page, err := admin.New(admin.Deps{
		Store: st, Log: log, Version: cfg.Version,
		People: hub, Links: links, Relay: rel, Updates: updates,
	})
	if err != nil {
		return nil, fmt.Errorf("the admin page's templates: %w", err)
	}
	s := &Server{Store: st, Log: log, Hub: hub, Links: links, Relay: rel, Admin: page, Console: handler, Updates: updates, Key: key, version: cfg.Version}
	s.console, err = net.Listen("tcp", cfg.ConsoleAddr)
	if err != nil {
		return nil, err
	}
	s.admin, err = net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		s.console.Close()
		return nil, err
	}
	return s, nil
}

// ConsoleAddress is where the console port ended up listening.
func (s *Server) ConsoleAddress() string { return s.console.Addr().String() }

// AdminAddress is where the admin page ended up listening.
func (s *Server) AdminAddress() string { return s.admin.Addr().String() }

func (s *Server) serve(l net.Listener, h http.Handler, longRequests bool) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	if !longRequests {
		srv.WriteTimeout = 30 * time.Second
	}
	s.servers = append(s.servers, srv)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		srv.Serve(l)
	}()
}

// Start opens both ports and the work that runs by itself. The update notice asks GitHub only if
// checkUpdates says so, which a test does not.
func (s *Server) Start(checkUpdates bool) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.Log.Started(s.version)
	s.serve(s.console, s.Console, true)
	s.serve(s.admin, s.Admin.Handler(), false)
	go s.Relay.Apply()
	for _, run := range []func(context.Context){s.Hub.Run, s.Links.Run} {
		s.wg.Add(1)
		go func(run func(context.Context)) {
			defer s.wg.Done()
			run(ctx)
		}(run)
	}
	if checkUpdates {
		go s.Updates.Run(ctx)
	}
}

// Stop closes the ports and saves what is waiting to be saved.
func (s *Server) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, srv := range s.servers {
		srv.Shutdown(ctx)
		srv.Close()
	}
	s.console.Close()
	s.admin.Close()
	s.wg.Wait()
	s.Relay.Close()
	s.Log.FlushCounts()
	err := s.Store.Flush()
	s.Log.Stopped()
	return err
}

// Port reads a port from text, for main.
func Port(text string, fallback int) (int, error) {
	if text == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("not a port: " + text)
	}
	return n, nil
}
