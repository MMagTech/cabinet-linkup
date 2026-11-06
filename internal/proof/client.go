package proof

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The largest request a server reads, and the largest answer a client reads.
const (
	MaxBody   = 64 * 1024
	MaxAnswer = 256 * 1024
)

// The bytes that say what a body is.
const (
	KindPairFirst  = 1
	KindPairSecond = 2
	KindOpen       = 3
	KindSealed     = 4
)

// Path is where everything is sent, under the address's base.
const Path = "/linkup/v1"

// What pairing can come to, besides not reaching anything.
var (
	ErrWrongCode    = errors.New("wrong or expired code")
	ErrTooManyTries = errors.New("too many tries")
	ErrRemoved      = errors.New("removed from the server")
	ErrUnreachable  = errors.New("can't reach that server")
)

// Hello is what each side states on connect.
type Hello struct {
	Kind     string `json:"kind,omitempty"`
	Protocol int    `json:"protocol"`
	Oldest   int    `json:"oldest"`
	Stream   int    `json:"stream,omitempty"`
	Name     string `json:"name,omitempty"`
	Session  string `json:"session,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	T        int64  `json:"t,omitempty"`
}

// PushedAddressOK says whether an address another server told this one could be tried at all: a
// host, a port and an optional path. PROTOCOL.md, "An address a server is told is only a candidate".
func PushedAddressOK(address string) bool {
	if address == "" || len(address) > 255 || strings.ContainsAny(address, "?#@ \\") {
		return false
	}
	for _, r := range address {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	a := address
	if i := strings.Index(a, "://"); i >= 0 {
		if scheme := a[:i]; scheme != "http" && scheme != "https" {
			return false
		}
		a = a[i+3:]
	}
	return a != "" && a[0] != '/'
}

// Bases turns the free text of an address into the places to try, as PROTOCOL.md's table says.
func Bases(address string) []string {
	a := strings.TrimSpace(address)
	a = strings.TrimRight(a, "/")
	if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
		return []string{a}
	}
	host := a
	path := ""
	if i := strings.Index(a, "/"); i >= 0 {
		host, path = a[:i], a[i:]
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return []string{"http://" + host + path}
	}
	return []string{"https://" + host + path, "http://" + host + path}
}

// Client is one side that joins and opens sessions: the stand-in console, and a server toward a
// linked server. It keeps one session and opens another whenever that one stops working.
type Client struct {
	static KeyPair
	http   *http.Client

	mu      sync.Mutex
	address string
	peerKey []byte
	base    string
	session *Session
	peer    Hello
	stream  int
	lastT   int64
	check   func(net.IP) error
	dialed  net.IP
}

// NewClient makes a client for an address. peer is the other side's remembered key, or nil before
// pairing.
func NewClient(address string, static KeyPair, peer []byte) *Client {
	c := &Client{static: static, address: address, peerKey: peer, stream: Stream}
	c.http = c.newHTTPClient()
	return c
}

// newHTTPClient makes the transport. A certificate is not what makes this safe, so a self-signed
// proxy works and none is checked: what comes back is proved by its seal. Redirects are never
// followed, so a server cannot send this one somewhere else.
func (c *Client) newHTTPClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: c.controlDial}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = insecureTLS()
	t.ResponseHeaderTimeout = 40 * time.Second
	t.DialContext = d.DialContext
	return &http.Client{
		Transport: t,
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// controlDial runs with the address a name resolved to, just before connecting, so a name cannot
// change to point somewhere else after it was checked.
func (c *Client) controlDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	c.mu.Lock()
	check := c.check
	c.dialed = ip
	c.mu.Unlock()
	if check != nil {
		return check(ip)
	}
	return nil
}

// SetDialCheck sets what may be connected to.
func (c *Client) SetDialCheck(check func(net.IP) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.check = check
}

// Dialed is the address last connected to.
func (c *Client) Dialed() net.IP {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialed
}

// Prove opens a session under the remembered key, to show an address really is that server.
func (c *Client) Prove(ctx context.Context) error {
	_, err := c.open(ctx)
	return err
}

// SetStream sets the stream version this side states.
func (c *Client) SetStream(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stream = n
}

// SetAddress points the client somewhere new. The key is what is checked, so nothing else changes.
func (c *Client) SetAddress(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.address != address {
		c.address = address
		c.base = ""
	}
}

// Peer is what the other side last stated.
func (c *Client) Peer() Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

func (c *Client) post(ctx context.Context, base string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+Path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil {
		return 0, nil, err
	}
	if len(out) > MaxAnswer {
		return 0, nil, errors.New("the answer is too long")
	}
	return resp.StatusCode, out, nil
}

// first tries each place the address could mean until one answers.
func (c *Client) first(ctx context.Context, body []byte) (string, int, []byte, error) {
	c.mu.Lock()
	address, base := c.address, c.base
	c.mu.Unlock()
	places := Bases(address)
	if base != "" {
		places = []string{base}
	}
	var last error = ErrUnreachable
	for _, place := range places {
		status, out, err := c.post(ctx, place, body)
		if err != nil {
			last = err
			continue
		}
		c.mu.Lock()
		c.base = place
		c.mu.Unlock()
		return place, status, out, nil
	}
	return "", 0, nil, fmt.Errorf("%w: %v", ErrUnreachable, last)
}

// Pair uses a code. On success it returns the other side's key, which is now remembered.
func (c *Client) Pair(ctx context.Context, tidy string, joining, reply any) ([]byte, error) {
	joiner, msg1, err := NewJoiner(SlowHash(tidy), c.static)
	if err != nil {
		return nil, err
	}
	base, status, out, err := c.first(ctx, append([]byte{KindPairFirst}, msg1...))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, ErrWrongCode
	case http.StatusTooManyRequests:
		return nil, ErrTooManyTries
	default:
		return nil, ErrUnreachable
	}
	if len(out) <= PairIDSize {
		return nil, ErrUnreachable
	}
	id, msg2 := out[:PairIDSize], out[PairIDSize:]
	joinBytes, err := json.Marshal(joining)
	if err != nil {
		return nil, err
	}
	hello, msg3, err := joiner.Second(msg2, joinBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var theirs Hello
	json.Unmarshal(hello, &theirs)

	body := append([]byte{KindPairSecond}, id...)
	status, out, err = c.post(ctx, base, append(body, msg3...))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, ErrWrongCode
	case http.StatusTooManyRequests:
		return nil, ErrTooManyTries
	default:
		return nil, ErrUnreachable
	}
	plain, key, err := joiner.Reply(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if reply != nil {
		if err := json.Unmarshal(plain, reply); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.peerKey = key
	c.peer = theirs
	c.session = nil
	c.mu.Unlock()
	return key, nil
}

// open starts a session with the remembered key.
func (c *Client) open(ctx context.Context) (*Session, error) {
	c.mu.Lock()
	peerKey, stream := c.peerKey, c.stream
	c.lastT = max(time.Now().UnixNano(), c.lastT+1)
	t := c.lastT
	c.mu.Unlock()
	if len(peerKey) != KeySize {
		return nil, errors.New("no key for that server")
	}
	payload, err := json.Marshal(Hello{Protocol: Protocol, Oldest: Oldest, Stream: stream, T: t})
	if err != nil {
		return nil, err
	}
	opening, msg, err := Open(c.static, peerKey, payload)
	if err != nil {
		return nil, err
	}
	_, status, out, err := c.first(ctx, append([]byte{KindOpen}, msg...))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: the server answered %d", ErrUnreachable, status)
	}
	plain, session, err := opening.Finish(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var theirs Hello
	if err := json.Unmarshal(plain, &theirs); err != nil {
		return nil, ErrUnreachable
	}
	if theirs.Removed {
		return nil, ErrRemoved
	}
	id, err := hex.DecodeString(theirs.Session)
	if err != nil || len(id) != SessionIDSize {
		return nil, ErrUnreachable
	}
	copy(session.ID[:], id)
	c.mu.Lock()
	c.peer = theirs
	c.session = session
	c.mu.Unlock()
	return session, nil
}

// Call sends one request in the session and reads its answer, opening a session if it has to, and
// once more if the one it had has gone, as after a restart of the other side.
func (c *Client) Call(ctx context.Context, request, reply any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	for attempt := range 2 {
		c.mu.Lock()
		session := c.session
		c.mu.Unlock()
		if session == nil {
			if session, err = c.open(ctx); err != nil {
				return err
			}
		}
		out, err := c.sealed(ctx, session, body)
		if err == nil {
			if reply == nil {
				return nil
			}
			return json.Unmarshal(out, reply)
		}
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
		if attempt == 1 || ctx.Err() != nil {
			return err
		}
	}
	return ErrUnreachable
}

func (c *Client) sealed(ctx context.Context, session *Session, plain []byte) ([]byte, error) {
	c.mu.Lock()
	sealed, counter := session.SealRequest(plain)
	base := c.base
	c.mu.Unlock()
	status, out, err := c.post(ctx, base, append([]byte{KindSealed}, sealed...))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: the server answered %d", ErrUnreachable, status)
	}
	return session.OpenAnswer(counter, out)
}
