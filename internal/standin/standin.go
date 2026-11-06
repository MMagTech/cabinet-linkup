// Package standin is a stand-in console. It speaks PROTOCOL.md as the real console's program must,
// holds nothing else, and is what the tests and the two-container check talk to a server with. The
// real console is C++ in another repository: this exists so the server can be proved without one.
package standin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/proof"
)

// Console is one stand-in console.
type Console struct {
	Key    proof.KeyPair
	Client *proof.Client
	Number int
	after  int
	epoch  string
}

// Friend is one line of a list, as a console hears it.
type Friend struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Server   string `json:"server"`
	Status   string `json:"status"`
	Key      []byte `json:"key"`
	Stream   int    `json:"stream"`
	Invite   *struct {
		ID     string `json:"id"`
		Sealed string `json:"sealed"`
	} `json:"invite"`
}

// Event is one thing a console hears.
type Event struct {
	Seq     int    `json:"seq"`
	Type    string `json:"type"`
	Invite  string `json:"invite"`
	From    string `json:"from"`
	Reason  string `json:"reason"`
	Session string `json:"session"`
	Sealed  string `json:"sealed"`
}

// New makes a console with a new key, not yet paired.
func New(address string) (*Console, error) {
	key, err := proof.NewKeyPair()
	if err != nil {
		return nil, err
	}
	return &Console{Key: key, Client: proof.NewClient(address, key, nil)}, nil
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 40*time.Second)
}

// Pair types a code on the console, with this account signed in.
func (c *Console) Pair(code, account, username string) error {
	tidy, ok := proof.TidyCode(code)
	if !ok {
		return proof.ErrWrongCode
	}
	var reply struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Console int    `json:"console"`
	}
	cx, cancel := ctx()
	defer cancel()
	_, err := c.Client.Pair(cx, tidy, map[string]any{
		"kind": "console", "protocol": proof.Protocol, "oldest": proof.Oldest, "stream": proof.Stream,
		"account": account, "username": username,
	}, &reply)
	if err != nil {
		return err
	}
	if !reply.OK {
		return fmt.Errorf("%w: %s", proof.ErrWrongCode, reply.Error)
	}
	c.Number = reply.Console
	return nil
}

// do sends one request and reads its answer into reply, and turns an "error" answer into an error.
func (c *Console) do(req map[string]any, reply func([]byte) error) error {
	cx, cancel := ctx()
	defer cancel()
	var raw rawAnswer
	if err := c.Client.Call(cx, req, &raw); err != nil {
		return err
	}
	var probe struct {
		Error string `json:"error"`
	}
	if err := raw.into(&probe); err == nil && probe.Error != "" {
		return errors.New(probe.Error)
	}
	if reply != nil {
		return reply(raw)
	}
	return nil
}

// Signin signs an account in.
func (c *Console) Signin(account, username, status string) (string, error) {
	var out struct {
		Person string `json:"person"`
	}
	err := c.do(map[string]any{"op": "signin", "account": account, "username": username, "status": status}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Person, err
}

// Signout leaves nobody signed in.
func (c *Console) Signout() error { return c.do(map[string]any{"op": "signout"}, nil) }

// Status sets the account's own line.
func (c *Console) Status(status string) error {
	return c.do(map[string]any{"op": "status", "status": status}, nil)
}

// Friends fetches the list.
func (c *Console) Friends() ([]Friend, error) {
	var out struct {
		Friends []Friend `json:"friends"`
	}
	err := c.do(map[string]any{"op": "friends"}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Friends, err
}

// Config fetches the server's name and STUN list.
func (c *Console) Config() (string, []string, error) {
	var out struct {
		Name string   `json:"name"`
		Stun []string `json:"stun"`
	}
	err := c.do(map[string]any{"op": "config"}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Name, out.Stun, err
}

// Wait is the one long request.
func (c *Console) Wait() ([]Event, error) {
	var out struct {
		Epoch  string  `json:"epoch"`
		Events []Event `json:"events"`
		Next   int     `json:"next"`
	}
	err := c.do(map[string]any{"op": "wait", "after": c.after}, func(b []byte) error { return jsonInto(b, &out) })
	if err != nil {
		return nil, err
	}
	if c.epoch != "" && c.epoch != out.Epoch {
		c.after = 0
	} else {
		c.after = out.Next
	}
	c.epoch = out.Epoch
	return out.Events, nil
}

// Seal seals something for another console, for one invite. The server only carries it.
func (c *Console) Seal(peer []byte, invite string, payload []byte) (string, error) {
	box, err := proof.SealBox(c.Key, peer, invite, payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(box), nil
}

// Open opens what another console sealed.
func (c *Console) Open(peer []byte, invite, sealed string) ([]byte, error) {
	box, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	return proof.OpenBox(c.Key, peer, invite, box)
}

// NewInviteID picks an invite's id, before anything is sealed for it.
func NewInviteID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Invite knocks on a friend, with an id the inviter picked and a box bound to it.
func (c *Console) Invite(id, friend, sealed string) (string, error) {
	var out struct {
		Invite string `json:"invite"`
	}
	err := c.do(map[string]any{"op": "invite", "invite": id, "friend": friend, "sealed": sealed}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Invite, err
}

// Cancel takes back an invite.
func (c *Console) Cancel(invite string) error {
	return c.do(map[string]any{"op": "cancel", "invite": invite}, nil)
}

// Decline answers an invite with no.
func (c *Console) Decline(invite string) error {
	return c.do(map[string]any{"op": "decline", "invite": invite}, nil)
}

// Join answers an invite with yes.
func (c *Console) Join(invite string) (string, error) {
	var out struct {
		Session string `json:"session"`
	}
	err := c.do(map[string]any{"op": "join", "invite": invite}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Session, err
}

// Signal passes sealed connection details on.
func (c *Console) Signal(session, sealed string) error {
	return c.do(map[string]any{"op": "signal", "session": session, "sealed": sealed}, nil)
}

// End ends a session.
func (c *Console) End(session string) error {
	return c.do(map[string]any{"op": "end", "session": session}, nil)
}

// Report says how a session connected.
func (c *Console) Report(session, path, reason string) error {
	return c.do(map[string]any{"op": "report", "session": session, "path": path, "reason": reason}, nil)
}

// RelayCreds is a way through a relay.
type RelayCreds struct {
	URLs     []string `json:"urls"`
	Username string   `json:"username"`
	Password string   `json:"password"`
}

// Relay asks for a way through a relay.
func (c *Console) Relay(session string) (*RelayCreds, string, error) {
	var out struct {
		Relay  *RelayCreds `json:"relay"`
		Reason string      `json:"reason"`
	}
	err := c.do(map[string]any{"op": "relay", "session": session}, func(b []byte) error { return jsonInto(b, &out) })
	return out.Relay, out.Reason, err
}

// Leave takes the console off the server.
func (c *Console) Leave() error { return c.do(map[string]any{"op": "leave"}, nil) }
