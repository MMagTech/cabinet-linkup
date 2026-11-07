// A stand-in console on the command line, for trying a real server by hand and for
// ci/run-containers.sh. It keeps its key in a file, and takes one action at a time.
//
//	standin pair <file> <address> <code> <account> <username>
//	standin address <file> <address>              what retyping the address does
//	standin <action> <file> [args]                friends, wait, invite, join, decline, cancel, status, signin, leave, relay, report
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/turn/v4"

	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/standin"
)

type saved struct {
	Private, Public, Server []byte
	Address                 string
}

func load(path string) (saved, *standin.Console, error) {
	var s saved
	data, err := os.ReadFile(path)
	if err != nil {
		return s, nil, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, nil, err
	}
	key := proof.KeyPair{Private: s.Private, Public: s.Public}
	return s, &standin.Console{Key: key, Client: proof.NewClient(s.Address, key, s.Server)}, nil
}

func save(path string, s saved) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func print(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: standin <action> <file> [args]")
	}
	action, file, rest := args[0], args[1], args[2:]
	need := func(n int) error {
		if len(rest) < n {
			return fmt.Errorf("%s needs %d arguments", action, n)
		}
		return nil
	}
	if action == "pair" {
		if err := need(4); err != nil {
			return err
		}
		c, err := standin.New(rest[0])
		if err != nil {
			return err
		}
		tidy, ok := proof.TidyCode(rest[1])
		if !ok {
			return proof.ErrWrongCode
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var reply struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Console int    `json:"console"`
		}
		key, err := c.Client.Pair(ctx, tidy, map[string]any{
			"kind": "console", "protocol": proof.Protocol, "oldest": proof.Oldest, "stream": proof.Stream,
			"account": rest[2], "username": rest[3],
		}, &reply)
		if err != nil {
			return err
		}
		if !reply.OK {
			return errors.New(reply.Error)
		}
		if err := save(file, saved{Private: c.Key.Private, Public: c.Key.Public, Server: key, Address: rest[0]}); err != nil {
			return err
		}
		return print(reply)
	}
	s, c, err := load(file)
	if err != nil {
		return err
	}
	switch action {
	case "address":
		if err := need(1); err != nil {
			return err
		}
		s.Address = rest[0]
		return save(file, s)
	case "signin":
		if err := need(2); err != nil {
			return err
		}
		id, err := c.Signin(rest[0], rest[1], "online")
		if err != nil {
			return err
		}
		return print(map[string]string{"person": id})
	case "status":
		if err := need(1); err != nil {
			return err
		}
		return c.Status(rest[0])
	case "friends":
		list, err := c.Friends()
		if err != nil {
			return err
		}
		return print(list)
	case "config":
		name, stun, err := c.Config()
		if err != nil {
			return err
		}
		return print(map[string]any{"name": name, "stun": stun})
	case "wait":
		events, err := c.Wait()
		if err != nil {
			return err
		}
		return print(events)
	case "invite":
		if err := need(2); err != nil {
			return err
		}
		id, err := c.Invite(standin.NewInviteID(), rest[0], rest[1])
		if err != nil {
			return err
		}
		return print(map[string]string{"invite": id})
	case "join":
		if err := need(1); err != nil {
			return err
		}
		id, err := c.Join(rest[0])
		if err != nil {
			return err
		}
		return print(map[string]string{"session": id})
	case "decline":
		if err := need(1); err != nil {
			return err
		}
		return c.Decline(rest[0])
	case "cancel":
		if err := need(1); err != nil {
			return err
		}
		return c.Cancel(rest[0])
	case "end":
		if err := need(1); err != nil {
			return err
		}
		return c.End(rest[0])
	case "report":
		if err := need(3); err != nil {
			return err
		}
		return c.Report(rest[0], rest[1], rest[2])
	case "relay":
		if err := need(1); err != nil {
			return err
		}
		creds, why, err := c.Relay(rest[0])
		if err != nil {
			return err
		}
		return print(map[string]any{"relay": creds, "reason": why})
	case "leave":
		return c.Leave()
	case "relaycheck":
		if err := need(2); err != nil {
			return err
		}
		return relayCheck(c, rest[0], rest[1])
	}
	return fmt.Errorf("no action %q", action)
}

// relayCheck sends real packets through a session's relay, to a peer that echoes them.
func relayCheck(c *standin.Console, session, peerAddress string) error {
	creds, why, err := c.Relay(session)
	if err != nil {
		return err
	}
	if creds == nil {
		return errors.New("no relay: " + why)
	}
	host := strings.TrimPrefix(strings.SplitN(creds.URLs[0], "?", 2)[0], "turn:")
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: host, TURNServerAddr: host, Username: creds.Username, Password: creds.Password,
		Realm: "linkup", Conn: conn, RTO: 200 * time.Millisecond,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		return err
	}
	allocation, err := client.Allocate()
	if err != nil {
		return fmt.Errorf("allocating: %w", err)
	}
	defer allocation.Close()
	peer, err := net.ResolveUDPAddr("udp4", peerAddress)
	if err != nil {
		return err
	}
	allocation.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload := make([]byte, 1200)
	var sent, echoed atomic.Int32
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		buffer := make([]byte, 2048)
		for range 50 {
			if _, err := allocation.WriteTo(payload, peer); err != nil {
				return
			}
			sent.Add(1)
			if n, _, err := allocation.ReadFrom(buffer); err == nil && n == len(payload) {
				echoed.Add(1)
			}
		}
	}()
	// A relay that has stopped answering, at its limit say, leaves a write waiting for good.
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
	}
	return print(map[string]any{"relayed_at": allocation.LocalAddr().String(), "sent": sent.Load(), "echoed": echoed.Load()})
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
