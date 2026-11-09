// Package relay is the last resort for a game with no direct path: Pion's TURN server,
// off until the owner turns it on, with what this project adds around it. A password
// for one session and no standing logins, a cap on each connection, the monthly limit and
// its reset day, and a refusal to relay to private addresses, which is what stops it being
// a way into its owner's home network.
//
// Game packets are encrypted between the two consoles, so nothing here can read them.
// Packets are held only long enough to send on. Nothing is written per packet: the count
// is saved now and then.
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v4"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	realm = "linkup"

	// A new connection can use a password this long after it was given out.
	newConnectionWindow = 10 * time.Minute
	// A connection already started lasts until the session ends, or this long.
	longestSession = 24 * time.Hour
	// One session's two consoles need one connection each, and a retry may need another.
	connectionsPerSession = 4
	// How many addresses one connection can send to.
	peersPerConnection = 8

	// Each connection's cap: the console's stream asks for 18 Mbit/s, and the hardware
	// reaches 23 to 25 for a second at a time.
	capBytesPerSecond = 26_000_000 / 8

	gigabyte = 1_000_000_000

	saveEvery    = time.Minute
	findEvery    = 5 * time.Minute
	testDeadline = 5 * time.Second
	stunDeadline = 3 * time.Second
)

// How many relayed games can run at once, which the owner picks on the admin page. Each game takes
// two ports at most, one for each console, right after the relay's own port, so the owner forwards
// one line: 3478 to 3482 for two games. The container maps the ports for MaxGames, and a port past
// the owner's number is never opened.
const (
	MaxGames     = 16
	DefaultGames = 2
	portsPerGame = 2
)

// GamesAtOnce is the owner's number as the relay uses it: a data folder from before it was asked
// for has none.
func GamesAtOnce(chosen int) int {
	if chosen < 1 {
		return DefaultGames
	}
	return min(chosen, MaxGames)
}

// Forward is what the owner forwards on their router for a number of games at once: the relay's
// port and the games' ports after it, as one range.
func Forward(port, games int) (first, last int) {
	return port, port + portsPerGame*GamesAtOnce(games)
}

// Config is what the owner sets when starting the container.
type Config struct {
	Port         int
	AllowPrivate bool // only for testing consoles on one home network; the log says so
	found        net.IP
}

// counts is the relay's file in the data folder.
type counts struct {
	PeriodStart time.Time
	Bytes       uint64
	Games       int
}

// Credentials are what a console is given for one session.
type Credentials struct {
	URLs     []string `json:"urls"`
	Username string   `json:"username"`
	Password string   `json:"password"`
}

// Status is what the admin page shows.
type Status struct {
	On          bool
	Found       string
	UsedBytes   uint64
	LimitBytes  uint64
	Games       int
	AtLimit     bool
	Resets      time.Time
	TestFailed  bool
	GamesAtOnce int
	FirstPort   int // what to forward, as one range
	LastPort    int
}

// Service runs the relay and counts what passes through it.
type Service struct {
	cfg   Config
	store *store.Store
	log   *logfile.Log

	used      atomic.Uint64
	limit     atomic.Uint64
	full      atomic.Bool
	running   atomic.Bool
	savedUsed uint64

	mu         sync.Mutex
	server     *turn.Server
	secret     []byte
	address    net.IP
	period     time.Time
	games      int
	addressNow atomic.Pointer[net.IP]
	admitted   map[string]bool            // username and source of a connection that has started
	ended      map[string]time.Time       // sessions the server has seen end, by tag
	conns      map[int]*metered           // by relay port
	allocs     map[string]int             // live connections per session, by tag
	peers      map[string]map[string]bool // peer addresses per client
	testFailed bool
	loggedFull bool
	cancel     context.CancelFunc
}

// New makes the service, reading its counts. Nothing listens until Apply.
func New(cfg Config, st *store.Store, log *logfile.Log) *Service {
	s := &Service{
		cfg: cfg, store: st, log: log,
		admitted: map[string]bool{},
		ended:    map[string]time.Time{},
		conns:    map[int]*metered{},
		allocs:   map[string]int{},
		peers:    map[string]map[string]bool{},
	}
	st.View(func(state *store.State) { s.secret = state.RelaySecret })
	var c counts
	if st.ReadJSON("relay.json", &c) == nil {
		s.used.Store(c.Bytes)
		s.savedUsed = c.Bytes
		s.period = c.PeriodStart
		s.games = c.Games
	}
	s.applyLimit(time.Now())
	return s
}

// PeriodStart is when the relay's month began, counted from the owner's reset day. A
// reset day past the end of a month means that month's last day.
func PeriodStart(now time.Time, resetDay int) time.Time {
	now = now.UTC()
	day := clampDay(now.Year(), now.Month(), resetDay)
	if now.Day() >= day {
		return time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
	}
	prev := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	return time.Date(prev.Year(), prev.Month(), clampDay(prev.Year(), prev.Month(), resetDay), 0, 0, 0, 0, time.UTC)
}

// NextReset is the next time the month starts over.
func NextReset(now time.Time, resetDay int) time.Time {
	now = now.UTC()
	day := clampDay(now.Year(), now.Month(), resetDay)
	if now.Day() < day {
		return time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
	}
	next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return time.Date(next.Year(), next.Month(), clampDay(next.Year(), next.Month(), resetDay), 0, 0, 0, 0, time.UTC)
}

func clampDay(year int, month time.Month, day int) int {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return min(max(day, 1), last)
}

// Apply brings the relay into line with the owner's settings, after a change or at the
// start, and looks up its address again. Off means the ports are closed.
func (s *Service) Apply() {
	var on bool
	s.store.View(func(st *store.State) { on = st.Settings.Relay })
	s.applyLimit(time.Now())
	s.mu.Lock()
	switch {
	case !on:
		s.stopLocked()
	case s.server == nil:
		if s.startLocked() != nil {
			s.log.Problem(logfile.ProblemRelayPort)
			on = false
		}
	}
	s.mu.Unlock()
	if on {
		s.findAddress()
	}
}

// Close stops the relay and saves the counts.
func (s *Service) Close() {
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	s.saveCounts()
}

func (s *Service) startLocked() error {
	conn, err := net.ListenPacket("udp", ":"+strconv.Itoa(s.cfg.Port))
	if err != nil {
		return err
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		AuthHandler:   s.auth,
		QuotaHandler:  s.quota,
		EventHandler:  s.events(),
		LoggerFactory: quiet{},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            conn,
			RelayAddressGenerator: generator{s},
			PermissionHandler:     s.permit,
		}},
	})
	if err != nil {
		conn.Close()
		return err
	}
	s.server = server
	s.running.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.keepUp(ctx)
	s.log.RelayOn()
	if s.cfg.AllowPrivate {
		s.log.RelayPrivateAllowed()
	}
	return nil
}

func (s *Service) stopLocked() {
	if s.server == nil {
		return
	}
	s.running.Store(false)
	s.cancel()
	s.server.Close()
	s.server = nil
	s.admitted = map[string]bool{}
	s.conns = map[int]*metered{}
	s.allocs = map[string]int{}
	s.peers = map[string]map[string]bool{}
	s.log.RelayOff()
}

// keepUp saves the count, starts the month over on its day, and follows the address.
func (s *Service) keepUp(ctx context.Context) {
	save := time.NewTicker(saveEvery)
	find := time.NewTicker(findEvery)
	defer save.Stop()
	defer find.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-save.C:
			s.applyLimit(time.Now())
			s.saveCounts()
		case <-find.C:
			s.findAddress()
		}
	}
}

// applyLimit starts a new month if one has begun, and reads the limit.
func (s *Service) applyLimit(now time.Time) {
	var limitGB, resetDay int
	s.store.View(func(st *store.State) {
		limitGB = st.Settings.RelayLimitGB
		resetDay = st.Settings.RelayResetDay
	})
	s.limit.Store(uint64(max(limitGB, 0)) * gigabyte)
	start := PeriodStart(now, resetDay)
	s.mu.Lock()
	if s.period.Before(start) {
		s.period = start
		s.used.Store(0)
		s.savedUsed = 0
		s.games = 0
		s.loggedFull = false
	}
	s.mu.Unlock()
	s.full.Store(s.used.Load() >= s.limit.Load())
	if s.full.Load() {
		s.mu.Lock()
		first := !s.loggedFull
		s.loggedFull = true
		s.mu.Unlock()
		if first {
			s.log.RelayLimit()
		}
	}
}

func (s *Service) saveCounts() {
	s.mu.Lock()
	used := s.used.Load()
	c := counts{PeriodStart: s.period, Bytes: used, Games: s.games}
	changed := used != s.savedUsed
	s.savedUsed = used
	s.mu.Unlock()
	if changed {
		if s.store.WriteJSON("relay.json", c) != nil {
			s.log.Problem(logfile.ProblemSave)
		}
	}
}

// AddGame counts one relayed game this month.
func (s *Service) AddGame() {
	s.mu.Lock()
	s.games++
	c := counts{PeriodStart: s.period, Bytes: s.used.Load(), Games: s.games}
	s.mu.Unlock()
	if s.store.WriteJSON("relay.json", c) != nil {
		s.log.Problem(logfile.ProblemSave)
	}
}

// Status is the relay as the admin page shows it.
func (s *Service) Status() Status {
	s.applyLimit(time.Now())
	var resetDay, games int
	var on bool
	s.store.View(func(st *store.State) {
		resetDay = st.Settings.RelayResetDay
		on = st.Settings.Relay
		games = GamesAtOnce(st.Settings.RelayGames)
	})
	first, last := Forward(s.cfg.Port, games)
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		On:          on,
		Found:       s.foundLocked(),
		UsedBytes:   s.used.Load(),
		LimitBytes:  s.limit.Load(),
		Games:       s.games,
		AtLimit:     s.full.Load(),
		Resets:      NextReset(time.Now(), resetDay),
		TestFailed:  s.testFailed,
		GamesAtOnce: games,
		FirstPort:   first,
		LastPort:    last,
	}
}

func (s *Service) foundLocked() string {
	if s.address == nil {
		return ""
	}
	return net.JoinHostPort(s.address.String(), strconv.Itoa(s.cfg.Port))
}

// Available says why a session cannot have the relay, or "" if it can.
func (s *Service) Available() string {
	switch {
	case !s.running.Load():
		return "off"
	case s.full.Load():
		return "limit"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.address == nil {
		return "off"
	}
	return ""
}

// Credentials gives out a password for one session.
func (s *Service) Credentials(session string) (Credentials, error) {
	if why := s.Available(); why != "" {
		return Credentials{}, errors.New(why)
	}
	s.mu.Lock()
	host := s.foundLocked()
	s.mu.Unlock()
	username := strconv.FormatInt(time.Now().Unix(), 10) + "." + s.tag(session)
	return Credentials{URLs: []string{"turn:" + host + "?transport=udp"}, Username: username, Password: s.password(username)}, nil
}

// EndSession stops a session's password and ends its connections as soon as the server sees the
// session end. Closing the socket is how a connection ends in Pion, and it has to happen here: by
// now a console's own request to end one is refused, and the connection would keep that console's
// port for the rest of its lifetime, turning away its next game from the same port.
func (s *Service) EndSession(session string) {
	now := time.Now()
	tag := s.tag(session)
	s.mu.Lock()
	s.ended[tag] = now
	for key := range s.admitted {
		if name, _, ok := strings.Cut(key, "|"); ok && tagOf(name) == tag {
			delete(s.admitted, key)
		}
	}
	var ending []*metered
	for _, c := range s.conns {
		if tagOf(c.username()) == tag {
			ending = append(ending, c)
		}
	}
	for id, at := range s.ended {
		if now.Sub(at) > longestSession {
			delete(s.ended, id)
		}
	}
	s.mu.Unlock()
	for _, c := range ending {
		c.Close()
	}
}

func (s *Service) password(username string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("relay password\x00" + username))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// tag stands for a session in a username, which travels in clear on UDP.
func (s *Service) tag(session string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("relay session\x00" + session))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

func tagOf(username string) string {
	_, tag, _ := strings.Cut(username, ".")
	return tag
}

// auth gives Pion the key to check a request's signature with. A new connection gets it
// inside its window. A connection that has started gets it until the session ends.
func (s *Service) auth(username, _ string, src net.Addr) ([]byte, bool) {
	if !s.running.Load() || s.full.Load() {
		return nil, false
	}
	issuedText, tag, ok := strings.Cut(username, ".")
	issued, err := strconv.ParseInt(issuedText, 10, 64)
	if !ok || err != nil || tag == "" {
		return nil, false
	}
	age := time.Since(time.Unix(issued, 0))
	if age < -time.Minute {
		return nil, false
	}
	s.mu.Lock()
	_, ended := s.ended[tag]
	started := s.admitted[username+"|"+src.String()]
	s.mu.Unlock()
	if ended || age > longestSession || (!started && age > newConnectionWindow) {
		return nil, false
	}
	return turn.GenerateAuthKey(username, realm, s.password(username)), true
}

func (s *Service) quota(username, _ string, _ net.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocs[tagOf(username)] < connectionsPerSession
}

// permit decides who a connection may send to: nothing private, and a few peers. A console has
// only a handful of public addresses, and this makes the relay no use for reaching anyone else.
func (s *Service) permit(client net.Addr, peer net.IP) bool {
	if !s.cfg.AllowPrivate && Blocked(peer) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := s.peers[client.String()]
	if known == nil {
		known = map[string]bool{}
		s.peers[client.String()] = known
	}
	if !known[peer.String()] && len(known) >= peersPerConnection {
		return false
	}
	known[peer.String()] = true
	return true
}

func (s *Service) events() turn.EventHandler {
	return turn.EventHandler{
		OnAuth: func(src, _ net.Addr, _, username, _, method string, verdict bool) {
			if verdict && method == "Allocate" {
				s.mu.Lock()
				s.admitted[username+"|"+src.String()] = true
				s.mu.Unlock()
			}
		},
		OnAllocationCreated: func(src, _ net.Addr, _, username, _ string, relayAddr net.Addr, _ int) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.allocs[tagOf(username)]++
			if c := s.conns[portOf(relayAddr)]; c != nil {
				c.setUsername(username)
			}
		},
		OnAllocationDeleted: func(src, _ net.Addr, _, username, _ string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.allocs[tagOf(username)]--
			if s.allocs[tagOf(username)] <= 0 {
				delete(s.allocs, tagOf(username))
			}
			delete(s.admitted, username+"|"+src.String())
			delete(s.peers, src.String())
		},
		OnPermissionCreated: func(_, _ net.Addr, _, _, _ string, relayAddr net.Addr, peer net.IP) {
			s.mu.Lock()
			c := s.conns[portOf(relayAddr)]
			s.mu.Unlock()
			if c != nil {
				c.allowPeer(peer)
			}
		},
		OnPermissionDeleted: func(_, _ net.Addr, _, _, _ string, relayAddr net.Addr, peer net.IP) {
			s.mu.Lock()
			c := s.conns[portOf(relayAddr)]
			s.mu.Unlock()
			if c != nil {
				c.dropPeer(peer)
			}
		},
	}
}

func portOf(addr net.Addr) int {
	if a, ok := addr.(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

// count adds to the month and says whether the limit still holds.
func (s *Service) count(n int) bool {
	if s.full.Load() {
		return false
	}
	if s.used.Add(uint64(n)) >= s.limit.Load() {
		s.full.Store(true)
		return false
	}
	return true
}

// generator hands Pion a socket for each connection, on one of the games' ports the owner's number
// allows, counted. A port is opened only for a connection and closed when it ends.
type generator struct{ s *Service }

func (g generator) Validate() error { return nil }

func (g generator) AllocatePacketConn(network string, _ int) (net.PacketConn, net.Addr, error) {
	s := g.s
	s.mu.Lock()
	address := s.address
	s.mu.Unlock()
	if address == nil {
		return nil, nil, errors.New("no address")
	}
	if (network == "udp4") != (address.To4() != nil) {
		return nil, nil, errors.New("no address of that family")
	}
	var games int
	s.store.View(func(st *store.State) { games = GamesAtOnce(st.Settings.RelayGames) })
	span := portsPerGame * games
	first, err := rand.Int(rand.Reader, big.NewInt(int64(span)))
	if err != nil {
		return nil, nil, err
	}
	for i := range span {
		port := s.cfg.Port + 1 + (int(first.Int64())+i)%span
		conn, err := net.ListenPacket(network, ":"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		m := newMetered(conn, s)
		s.mu.Lock()
		s.conns[port] = m
		s.mu.Unlock()
		return m, &net.UDPAddr{IP: address, Port: port}, nil
	}
	return nil, nil, errors.New("no port free")
}

func (g generator) AllocateConn(string, int) (net.Conn, net.Addr, error) {
	return nil, nil, errors.New("TCP is not relayed")
}

// metered is one connection's socket: its cap, its share of the month, and who may reach
// it. A packet from anyone without a permission is dropped before it is counted, so a
// stranger cannot use up an owner's limit by sending at an open port.
type metered struct {
	net.PacketConn
	s    *Service
	port int

	mu     sync.Mutex
	name   string
	peers  map[string]bool
	tokens float64
	last   time.Time
}

func newMetered(conn net.PacketConn, s *Service) *metered {
	port := 0
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		port = a.Port
	}
	return &metered{PacketConn: conn, s: s, port: port, peers: map[string]bool{}, tokens: capBytesPerSecond, last: time.Now()}
}

func (m *metered) Close() error {
	m.s.mu.Lock()
	// Pion closes it again when the connection ends, and the port may be another's by then.
	if m.s.conns[m.port] == m {
		delete(m.s.conns, m.port)
	}
	m.s.mu.Unlock()
	return m.PacketConn.Close()
}

func (m *metered) username() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.name
}

func (m *metered) setUsername(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.name = name
}

func (m *metered) allowPeer(ip net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers[ip.String()] = true
}

func (m *metered) dropPeer(ip net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, ip.String())
}

// allow takes n bytes from this connection's cap and from the month.
func (m *metered) allow(n int) bool {
	m.mu.Lock()
	now := time.Now()
	m.tokens = min(m.tokens+now.Sub(m.last).Seconds()*capBytesPerSecond, capBytesPerSecond)
	m.last = now
	ok := m.tokens >= float64(n)
	if ok {
		m.tokens -= float64(n)
	}
	m.mu.Unlock()
	return ok && m.s.count(n)
}

func (m *metered) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := m.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		a, isUDP := addr.(*net.UDPAddr)
		if !isUDP {
			continue
		}
		m.mu.Lock()
		known := m.peers[a.IP.String()]
		m.mu.Unlock()
		if known && m.allow(n) {
			return n, addr, nil
		}
	}
}

func (m *metered) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !m.s.sendable(addr) || !m.allow(len(p)) {
		return len(p), nil
	}
	return m.PacketConn.WriteTo(p, addr)
}

// sendable refuses ports under 1024, and the owner's own public address except on the relay's own
// ports: it leads to every port the owner forwards, and two consoles can share one relay.
func (s *Service) sendable(addr net.Addr) bool {
	a, ok := addr.(*net.UDPAddr)
	if !ok || a.Port < 1024 {
		return false
	}
	if own := s.addressNow.Load(); own != nil && a.IP.Equal(*own) {
		return a.Port >= s.cfg.Port && a.Port <= s.cfg.Port+portsPerGame*MaxGames
	}
	return true
}

// quiet keeps Pion's own log out of Docker's: it would print internet addresses.
type quiet struct{}

func (quiet) NewLogger(string) logging.LeveledLogger { return silent{} }

type silent struct{}

func (silent) Trace(string)                  {}
func (silent) Tracef(string, ...interface{}) {}
func (silent) Debug(string)                  {}
func (silent) Debugf(string, ...interface{}) {}
func (silent) Info(string)                   {}
func (silent) Infof(string, ...interface{})  {}
func (silent) Warn(string)                   {}
func (silent) Warnf(string, ...interface{})  {}
func (silent) Error(string)                  {}
func (silent) Errorf(string, ...interface{}) {}

// privateRanges are refused as well as what net/netip already knows about: anything that
// can lead back into a network and is not the public internet.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// Blocked says whether the relay refuses to send to an address, on IPv4 and IPv6 alike,
// including an IPv4 address written inside an IPv6 one.
func Blocked(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range privateRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// HostOf finds the host in the free text of a public address: a name, an IP, IPv6 in
// brackets, with or without a scheme, a port or a path.
func HostOf(address string) string {
	a := strings.TrimSpace(address)
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexAny(a, "/?#"); i >= 0 {
		a = a[:i]
	}
	if host, _, err := net.SplitHostPort(a); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(a, "[]")
}

// findAddress works out the address to hand to consoles: where the STUN list sees this server's
// packets come from. A relayed game's packets leave the same way, and nothing in front of the console
// port can change that answer. The public address's name is no guide: behind a reverse proxy, or a
// service like Cloudflare, it leads to whatever answers for it. IPv4 first, then IPv6 for a server
// with no IPv4.
//
// A test server for consoles on one home network takes its public address's own name first: there it
// is the server's local address, the one those consoles reach.
func (s *Service) findAddress() {
	if s.cfg.found != nil {
		s.setAddress(s.cfg.found)
		return
	}
	var public string
	var stunList []string
	s.store.View(func(st *store.State) {
		public = st.Settings.Address
		for _, x := range st.Settings.Stun {
			stunList = append(stunList, x.Address)
		}
	})
	if s.cfg.AllowPrivate {
		if ip := s.fromName(HostOf(public)); ip != nil {
			s.setAddress(ip)
			return
		}
	}
	for _, network := range []string{"udp4", "udp6"} {
		for _, server := range stunList {
			if ip := askStun(network, server); ip != nil && (s.cfg.AllowPrivate || !Blocked(ip)) {
				s.setAddress(ip)
				return
			}
		}
	}
	s.setAddress(nil)
	s.log.Problem(logfile.ProblemRelayAddr)
}

func (s *Service) setAddress(ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.address = ip
	if ip == nil {
		s.addressNow.Store(nil)
	} else {
		s.addressNow.Store(&ip)
	}
}

// fromName looks up the public address's host for a test server, IPv4 first.
func (s *Service) fromName(host string) net.IP {
	if host == "" {
		return nil
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		found, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil
		}
		ips = found
	}
	var v6 net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip
		}
		v6 = ip
	}
	return v6
}

// askStun asks one STUN server, over IPv4 or IPv6, what address this server's packets come from. It
// sends again while no answer comes, as UDP may lose one.
func askStun(network, server string) net.IP {
	to, err := net.ResolveUDPAddr(network, server)
	if err != nil {
		return nil
	}
	conn, err := net.ListenPacket(network, ":0")
	if err != nil {
		return nil
	}
	defer conn.Close()
	request, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if err != nil {
		return nil
	}
	deadline := time.Now().Add(stunDeadline)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		if _, err := conn.WriteTo(request.Raw, to); err != nil {
			return nil
		}
		wait := time.Now().Add(stunDeadline / 6)
		if wait.After(deadline) {
			wait = deadline
		}
		conn.SetReadDeadline(wait)
		for {
			n, from, err := conn.ReadFrom(buf)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			if err != nil {
				return nil
			}
			if a, ok := from.(*net.UDPAddr); !ok || !a.IP.Equal(to.IP) || a.Port != to.Port {
				continue
			}
			answer := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if answer.Decode() != nil || answer.Type != stun.BindingSuccess || answer.TransactionID != request.TransactionID {
				continue
			}
			var mapped stun.XORMappedAddress
			if mapped.GetFrom(answer) == nil {
				return mapped.IP
			}
			var plain stun.MappedAddress
			if plain.GetFrom(answer) == nil {
				return plain.IP
			}
		}
	}
	return nil
}

// Test is the self-check on the admin page: the server reaches its own public address from
// inside and asks for a relay, as a console would. A router that cannot loop back fails it
// wrongly, which the setup guide says; a friend's game is the final answer.
func (s *Service) Test() (time.Duration, error) {
	s.findAddress()
	took, err := s.test()
	s.mu.Lock()
	s.testFailed = err != nil
	s.mu.Unlock()
	s.log.RelayTest(err == nil)
	return took, err
}

func (s *Service) test() (time.Duration, error) {
	creds, err := s.Credentials(store.NewID())
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	host := s.foundLocked()
	s.mu.Unlock()
	network := "udp4"
	if strings.HasPrefix(host, "[") {
		network = "udp6"
	}
	conn, err := net.ListenPacket(network, ":0")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: host, TURNServerAddr: host,
		Username: creds.Username, Password: creds.Password, Realm: realm,
		Conn: conn, LoggerFactory: quiet{}, RTO: 200 * time.Millisecond,
	})
	if err != nil {
		return 0, err
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		return 0, err
	}
	type result struct {
		took time.Duration
		err  error
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		if _, err := client.SendBindingRequest(); err != nil {
			done <- result{err: err}
			return
		}
		took := time.Since(start)
		allocation, err := client.Allocate()
		if err != nil {
			done <- result{err: err}
			return
		}
		allocation.Close()
		done <- result{took: took}
	}()
	select {
	case r := <-done:
		return r.took, r.err
	case <-time.After(testDeadline):
		return 0, fmt.Errorf("no answer in %v", testDeadline)
	}
}
