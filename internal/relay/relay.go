// Package relay is the last resort for a game with no direct path: Pion's TURN server,
// off until the owner turns it on, with what this project adds around it. A password
// for one session and no standing logins, a cap on each connection, the monthly limit and
// its reset day, and a refusal to relay to private addresses, which is what stops it being
// a way into its owner's home network.
//
// Game packets are encrypted between the two consoles, so nothing here can read them.
// Packets are held only long enough to send on. Nothing is written per packet: the count
// is saved now and then.
//
// Each file has one job:
//
//   - relay.go: the service, its passwords, its ports and its month
//   - guard.go: who a relayed connection may reach, its cap, and the private addresses it never may
//   - address.go: finding the address to hand consoles, and the self-check on the admin page
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
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
	admitted   map[string]bool            // username and source of a connection that has a port
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

func (s *Service) events() turn.EventHandler {
	return turn.EventHandler{
		// A connection has started once it has a port. Pion reports a password that checks out before
		// the session's cap is applied, so one the cap turns away must not count: it would keep an
		// entry for every try, and could open past its window once a place came free.
		OnAllocationCreated: func(src, _ net.Addr, _, username, _ string, relayAddr net.Addr, _ int) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.admitted[username+"|"+src.String()] = true
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
