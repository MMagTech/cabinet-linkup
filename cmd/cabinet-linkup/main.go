// The LinkUp server. Everything it needs comes from the environment, and every setting an owner
// changes while it runs is on the admin page. PROTOCOL.md says what it speaks, and README.md is the
// setup guide.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/MMagTech/cabinet-linkup/internal/relay"
	"github.com/MMagTech/cabinet-linkup/internal/server"
)

// version is set when a release is built.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	consolePort, err := server.Port(os.Getenv("LINKUP_CONSOLE_PORT"), 7845)
	if err != nil {
		return err
	}
	adminPort, err := server.Port(os.Getenv("LINKUP_ADMIN_PORT"), 7846)
	if err != nil {
		return err
	}
	relayPort, err := server.Port(os.Getenv("LINKUP_RELAY_PORT"), 3478)
	if err != nil {
		return err
	}
	if _, last := relay.Forward(relayPort, relay.MaxGames); last > 65535 {
		return fmt.Errorf("LINKUP_RELAY_PORT leaves no room for the games' ports after it: %d", relayPort)
	}
	// 0.1.0 took the games' ports from here. They follow the relay's own port now, so the owner forwards
	// one range, and the admin page's Relay tab says which.
	if os.Getenv("LINKUP_RELAY_PORTS") != "" {
		fmt.Fprintln(os.Stderr, "LINKUP_RELAY_PORTS is no longer read: the relay's games use the ports right after LINKUP_RELAY_PORT")
	}
	data := os.Getenv("LINKUP_DATA")
	if data == "" {
		data = "/data"
	}
	private := os.Getenv("LINKUP_TEST_RELAY_PRIVATE") == "1"
	s, err := server.New(server.Config{
		Data:         data,
		ConsoleAddr:  net.JoinHostPort("", strconv.Itoa(consolePort)),
		AdminAddr:    net.JoinHostPort("", strconv.Itoa(adminPort)),
		RelayPort:    relayPort,
		RelayPrivate: private,
		Version:      version,
		Out:          os.Stdout,
	})
	if err != nil {
		return err
	}
	s.Start(true)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	return s.Stop()
}
