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
	"strings"
	"syscall"

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
	low, high, err := portRange(os.Getenv("LINKUP_RELAY_PORTS"), 49160, 49200)
	if err != nil {
		return err
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
		RelayMin:     low,
		RelayMax:     high,
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

func portRange(text string, low, high int) (int, int, error) {
	if text == "" {
		return low, high, nil
	}
	a, b, ok := strings.Cut(text, "-")
	if !ok {
		return 0, 0, fmt.Errorf("not a range of ports: %s", text)
	}
	first, err := server.Port(a, 0)
	if err != nil {
		return 0, 0, err
	}
	last, err := server.Port(b, 0)
	if err != nil {
		return 0, 0, err
	}
	if first > last {
		return 0, 0, fmt.Errorf("not a range of ports: %s", text)
	}
	return first, last, nil
}
