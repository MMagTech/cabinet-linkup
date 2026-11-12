package relay

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/turn/v4"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "100.127.255.255", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1", "203.0.113.5",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "2001:db8::1", "3fff::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:192.168.0.1", "::ffff:100.64.0.1",
		"64:ff9b::a00:1", "2002:a00:1::", "2001:0:a00:1::", "fec0::1", "::a00:1",
	}
	for _, a := range blocked {
		if !Blocked(net.ParseIP(a)) {
			t.Errorf("%s must be refused", a)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "100.63.255.255", "100.128.0.1", "172.32.0.1", "2606:4700:4700::1111", "3fff:1000::1", "::ffff:8.8.8.8"}
	for _, a := range allowed {
		if Blocked(net.ParseIP(a)) {
			t.Errorf("%s is on the internet", a)
		}
	}
	if !Blocked(nil) {
		t.Error("no address at all must be refused")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"linkup.example.net:7845":           "linkup.example.net",
		"linkup.example.net":                "linkup.example.net",
		"203.0.113.5:7845":                  "203.0.113.5",
		"[2001:db8::1]:7845":                "2001:db8::1",
		"[2001:db8::1]":                     "2001:db8::1",
		"https://proxy.example.net/linkup":  "proxy.example.net",
		"http://proxy.example.net:8080/x?y": "proxy.example.net",
		"  name.tailnet.ts.net:7845 ":       "name.tailnet.ts.net",
		"100.100.1.2:7845":                  "100.100.1.2",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

func TestMonthCountedFromResetDay(t *testing.T) {
	for _, c := range []struct {
		now         time.Time
		day         int
		start, next string
	}{
		{date(2026, 11, 6), 1, "2026-11-01", "2026-12-01"},
		{date(2026, 11, 1), 1, "2026-11-01", "2026-12-01"},
		{date(2026, 11, 6), 15, "2026-10-15", "2026-11-15"},
		{date(2026, 11, 15), 15, "2026-11-15", "2026-12-15"},
		{date(2026, 12, 20), 15, "2026-12-15", "2027-01-15"},
		{date(2027, 1, 3), 15, "2026-12-15", "2027-01-15"},
		{date(2027, 2, 28), 31, "2027-02-28", "2027-03-31"},
		{date(2027, 2, 27), 31, "2027-01-31", "2027-02-28"},
		{date(2028, 2, 29), 30, "2028-02-29", "2028-03-30"},
	} {
		if got := PeriodStart(c.now, c.day).Format("2006-01-02"); got != c.start {
			t.Errorf("start %v day %d = %s, want %s", c.now, c.day, got, c.start)
		}
		if got := NextReset(c.now, c.day).Format("2006-01-02"); got != c.next {
			t.Errorf("next %v day %d = %s, want %s", c.now, c.day, got, c.next)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// relayPort finds a relay port with every games' port after it free too, and one more past them.
func relayPort(t *testing.T) int {
	t.Helper()
	for range 50 {
		base := freePort(t)
		if base+portsPerGame*MaxGames+1 > 65535 {
			continue
		}
		var held []net.PacketConn
		free := true
		for p := base; p <= base+portsPerGame*MaxGames+1 && free; p++ {
			c, err := net.ListenPacket("udp", ":"+strconv.Itoa(p))
			if err != nil {
				free = false
				break
			}
			held = append(held, c)
		}
		for _, c := range held {
			c.Close()
		}
		if free {
			return base
		}
	}
	t.Fatal("no run of free ports")
	return 0
}

type rig struct {
	s     *Service
	store *store.Store
	host  string
	dir   string
}

func newRig(t *testing.T, allowPrivate bool, limitGB int) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, func(state *store.State) error {
		state.RelaySecret = []byte("0123456789abcdef0123456789abcdef")
		state.Settings.Relay = true
		state.Settings.RelayLimitGB = limitGB
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(state *store.State) error {
		state.Settings.RelayLimitGB = limitGB
		state.Settings.Relay = true
		return nil
	})
	log, err := logfile.Open(filepath.Join(dir, "log.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	port := relayPort(t)
	s := New(Config{Port: port, AllowPrivate: allowPrivate, found: net.IPv4(127, 0, 0, 1)}, st, log)
	s.Apply()
	t.Cleanup(s.Close)
	waitFor(t, func() bool { return s.Available() == "" })
	return &rig{s: s, store: st, host: "127.0.0.1:" + fmtInt(port), dir: dir}
}

func fmtInt(n int) string { return strconv.Itoa(n) }

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("it never happened")
}

func (r *rig) client(t *testing.T, c Credentials) *turn.Client {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return r.clientOn(t, conn, c)
}

func (r *rig) clientOn(t *testing.T, conn net.PacketConn, c Credentials) *turn.Client {
	t.Helper()
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: r.host, TURNServerAddr: r.host,
		Username: c.Username, Password: c.Password, Realm: realm,
		Conn: conn, LoggerFactory: quiet{}, RTO: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPrivatePeerRefused(t *testing.T) {
	r := newRig(t, false, 500)
	creds, err := r.s.Credentials("game1")
	if err != nil {
		t.Fatal(err)
	}
	alloc, err := r.client(t, creds).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer alloc.Close()
	for _, peer := range []string{"127.0.0.1:9", "10.0.0.1:9", "192.168.1.1:9", "169.254.1.1:9", "100.64.0.1:9", "[::1]:9", "[fc00::1]:9", "[fe80::1]:9", "[::ffff:192.168.1.1]:9"} {
		addr, err := net.ResolveUDPAddr("udp", peer)
		if err != nil {
			t.Fatal(err)
		}
		if err := alloc.(interface {
			CreatePermissions(...net.Addr) error
		}).CreatePermissions(addr); err == nil {
			t.Errorf("a permission for %s must be refused", peer)
		}
	}
	addr, _ := net.ResolveUDPAddr("udp", "8.8.8.8:53")
	if err := alloc.(interface {
		CreatePermissions(...net.Addr) error
	}).CreatePermissions(addr); err != nil {
		t.Errorf("a public peer is fine: %v", err)
	}
}

func TestLowPortsAndTheOwnersOwnAddress(t *testing.T) {
	r := newRig(t, true, 500)
	r.s.setAddress(net.IPv4(203, 0, 113, 24))
	own := net.IPv4(203, 0, 113, 24)
	for _, c := range []struct {
		ip   net.IP
		port int
		ok   bool
	}{
		{net.IPv4(8, 8, 8, 8), 53, false},
		{net.IPv4(8, 8, 8, 8), 123, false},
		{net.IPv4(8, 8, 8, 8), 1023, false},
		{net.IPv4(8, 8, 8, 8), 1024, true},
		{net.IPv4(8, 8, 8, 8), 50000, true},
		{own, 8080, false},
		{own, r.s.cfg.Port, true},
		{own, r.s.cfg.Port + 1, true},
		{own, r.s.cfg.Port + portsPerGame*MaxGames, true},
		{own, r.s.cfg.Port + portsPerGame*MaxGames + 1, false},
	} {
		if got := r.s.sendable(&net.UDPAddr{IP: c.ip, Port: c.port}); got != c.ok {
			t.Errorf("%v:%d = %v", c.ip, c.port, got)
		}
	}
}

func TestPeersPerConnection(t *testing.T) {
	r := newRig(t, true, 500)
	client := &net.UDPAddr{IP: net.IPv4(8, 8, 4, 4), Port: 5000}
	for i := range peersPerConnection {
		if !r.s.permit(client, net.IPv4(1, 1, 1, byte(i))) {
			t.Fatalf("peer %d refused", i)
		}
	}
	if r.s.permit(client, net.IPv4(1, 1, 2, 1)) {
		t.Error("too many peers")
	}
	if !r.s.permit(client, net.IPv4(1, 1, 1, 0)) {
		t.Error("one it already has is still fine")
	}
}

// A session cannot get more connections by asking for more passwords.
func TestConnectionsCountPerSessionNotPerPassword(t *testing.T) {
	r := newRig(t, true, 500)
	for i := 0; i <= connectionsPerSession+1; i++ {
		creds, _ := r.s.Credentials("game1")
		name := creds.Username
		r.s.mu.Lock()
		r.s.allocs[tagOf(name)] = i
		r.s.mu.Unlock()
		if got := r.s.quota(name, realm, nil); got != (i < connectionsPerSession) {
			t.Errorf("%d connections: %v", i, got)
		}
	}
}

// A connection has started once it has a port, and only then may it outlive the password's ten
// minutes. One the session's cap turned away has not started: counted as started, every refused try
// kept an entry, and could open past the window once a place came free.
func TestARefusedConnectionHasNotStarted(t *testing.T) {
	r := newRig(t, true, 500)
	creds, _ := r.s.Credentials("game1")
	tag := tagOf(creds.Username)
	r.s.mu.Lock()
	r.s.allocs[tag] = connectionsPerSession
	r.s.mu.Unlock()
	for range 3 {
		if _, err := r.client(t, creds).Allocate(); err == nil {
			t.Fatal("a connection past the session's cap must be refused")
		}
	}
	r.s.mu.Lock()
	refused := len(r.s.admitted)
	delete(r.s.allocs, tag)
	r.s.mu.Unlock()
	if refused != 0 {
		t.Errorf("%d refused connections count as started", refused)
	}
	alloc, err := r.client(t, creds).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer alloc.Close()
	r.s.mu.Lock()
	started := len(r.s.admitted)
	r.s.mu.Unlock()
	if started != 1 {
		t.Errorf("a connection with a port: %d started", started)
	}
}

func TestUsernameDoesNotCarryTheSession(t *testing.T) {
	r := newRig(t, true, 500)
	creds, _ := r.s.Credentials("deadbeefdeadbeef")
	if strings.Contains(creds.Username, "deadbeef") {
		t.Error(creds.Username)
	}
}

func TestWrongPasswordAndMadeUpNames(t *testing.T) {
	r := newRig(t, true, 500)
	creds, _ := r.s.Credentials("game1")
	bad := creds
	bad.Password = "guess"
	if _, err := r.client(t, bad).Allocate(); err == nil {
		t.Error("a wrong password must fail")
	}
	for _, name := range []string{"nonsense", "x.game1", "." + "game1", "1.", strings.Repeat("9", 40) + ".game1"} {
		if _, err := r.client(t, Credentials{Username: name, Password: "guess"}).Allocate(); err == nil {
			t.Errorf("%q must fail", name)
		}
	}
}

func TestPasswordIsForOneSession(t *testing.T) {
	r := newRig(t, true, 500)
	one, _ := r.s.Credentials("game1")
	two, _ := r.s.Credentials("game2")
	if one.Password == two.Password {
		t.Error("each session has its own password")
	}
	crossed := Credentials{Username: two.Username, Password: one.Password}
	if _, err := r.client(t, crossed).Allocate(); err == nil {
		t.Error("one session's password must not open another")
	}
}

func TestOldPasswordRefusedForNewConnection(t *testing.T) {
	r := newRig(t, true, 500)
	old := fmtInt(int(time.Now().Add(-newConnectionWindow-time.Minute).Unix())) + ".game1"
	creds := Credentials{Username: old, Password: r.s.password(old)}
	if _, err := r.client(t, creds).Allocate(); err == nil {
		t.Error("a password past its window must not open a new connection")
	}
}

func TestEndedSessionStops(t *testing.T) {
	r := newRig(t, true, 500)
	creds, _ := r.s.Credentials("game1")
	client := r.client(t, creds)
	alloc, err := client.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer alloc.Close()
	r.s.EndSession("game1")
	if _, err := r.client(t, creds).Allocate(); err == nil {
		t.Error("a session that ended must not open a connection")
	}
	waitFor(t, func() bool { return r.s.server.AllocationCount() == 0 })
}

// A console holds only a few ports, so its next game can come from the one its last game used.
func TestEndedSessionFreesThePort(t *testing.T) {
	r := newRig(t, true, 500)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := conn.LocalAddr().String()
	first, _ := r.s.Credentials("game1")
	if _, err := r.clientOn(t, conn, first).Allocate(); err != nil {
		t.Fatal(err)
	}
	r.s.EndSession("game1")
	waitFor(t, func() bool { return r.s.server.AllocationCount() == 0 })
	conn.Close()
	again, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	next, _ := r.s.Credentials("game2")
	alloc, err := r.clientOn(t, again, next).Allocate()
	if err != nil {
		t.Fatalf("the next game from the same port was refused: %v", err)
	}
	defer alloc.Close()
}

func TestCountsAndLimit(t *testing.T) {
	r := newRig(t, true, 1)
	r.s.setAddress(net.IPv4(127, 0, 0, 9))
	creds, _ := r.s.Credentials("game1")
	alloc, err := r.client(t, creds).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer alloc.Close()
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	payload := make([]byte, 1000)
	if _, err := alloc.WriteTo(payload, peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2000)
	n, from, err := peer.ReadFrom(buf)
	if err != nil || n != 1000 {
		t.Fatalf("relayed %d, %v", n, err)
	}
	if got := r.s.used.Load(); got != 1000 {
		t.Errorf("counted %d", got)
	}

	if _, err := peer.WriteTo(payload, from); err != nil {
		t.Fatal(err)
	}
	alloc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := alloc.ReadFrom(buf); err != nil {
		t.Fatalf("back: %v", err)
	}
	if got := r.s.used.Load(); got != 2000 {
		t.Errorf("counted %d both ways", got)
	}

	stranger, _ := net.ListenPacket("udp4", "127.0.0.2:0")
	if stranger != nil {
		defer stranger.Close()
		stranger.WriteTo(payload, from)
		time.Sleep(100 * time.Millisecond)
		if got := r.s.used.Load(); got != 2000 {
			t.Errorf("a stranger's packets must not count: %d", got)
		}
	}

	r.s.used.Store(gigabyte - 1)
	alloc.WriteTo(payload, peer.LocalAddr())
	waitFor(t, func() bool { return r.s.full.Load() })
	if why := r.s.Available(); why != "limit" {
		t.Errorf("available: %q", why)
	}
	if _, err := r.s.Credentials("game2"); err == nil {
		t.Error("no password while the limit is reached")
	}
	if _, err := r.client(t, creds).Allocate(); err == nil {
		t.Error("no new connection while the limit is reached")
	}
	st := r.s.Status()
	if !st.AtLimit || st.UsedBytes < gigabyte-1 {
		t.Errorf("status %+v", st)
	}
}

func TestCapDropsPastThePeak(t *testing.T) {
	r := newRig(t, true, 500)
	conn, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	m := newMetered(conn, r.s)
	defer conn.Close()
	sent := 0
	for m.allow(1200) {
		sent += 1200
		if sent > capBytesPerSecond*3 {
			t.Fatal("no cap")
		}
	}
	if float64(sent) < capBytesPerSecond*0.9 {
		t.Errorf("a one-second peak must pass: %d", sent)
	}
}

func TestMonthStartsOver(t *testing.T) {
	r := newRig(t, true, 1)
	r.s.used.Store(gigabyte)
	r.s.AddGame()
	r.s.applyLimit(time.Now())
	if !r.s.full.Load() {
		t.Fatal("should be full")
	}
	r.s.applyLimit(time.Now().AddDate(0, 2, 0))
	if r.s.used.Load() != 0 || r.s.full.Load() || r.s.Status().Games != 0 {
		t.Error("a new month starts at nothing")
	}
}

func TestCountsSurviveARestart(t *testing.T) {
	r := newRig(t, true, 500)
	r.s.used.Store(12345)
	r.s.AddGame()
	r.s.saveCounts()
	log, _ := logfile.Open(filepath.Join(r.dir, "log2.txt"), nil)
	again := New(Config{Port: freePort(t)}, r.store, log)
	if again.used.Load() != 12345 || again.games != 1 {
		t.Errorf("used %d games %d", again.used.Load(), again.games)
	}
}

func TestOffClosesThePorts(t *testing.T) {
	r := newRig(t, true, 500)
	port := r.s.cfg.Port
	if c, err := net.ListenPacket("udp", ":"+fmtInt(port)); err == nil {
		c.Close()
		t.Fatal("on means its port is held")
	}
	r.store.Update(func(st *store.State) error { st.Settings.Relay = false; return nil })
	r.s.Apply()
	if r.s.Available() != "off" {
		t.Error("available while off")
	}
	c, err := net.ListenPacket("udp", ":"+fmtInt(port))
	if err != nil {
		t.Fatalf("off means its port is free: %v", err)
	}
	c.Close()
	r.store.Update(func(st *store.State) error { st.Settings.Relay = true; return nil })
	r.s.Apply()
	waitFor(t, func() bool { return r.s.Available() == "" })
}

func TestSelfCheck(t *testing.T) {
	r := newRig(t, true, 500)
	took, err := r.s.Test()
	if err != nil {
		t.Fatalf("the check failed: %v", err)
	}
	if took <= 0 || r.s.Status().TestFailed {
		t.Errorf("took %v, %+v", took, r.s.Status())
	}
	data, _ := os.ReadFile(filepath.Join(r.dir, "log.txt"))
	if !strings.Contains(string(data), "relay test passed") {
		t.Error(string(data))
	}
}

func TestSelfCheckFailsWithNothingToReach(t *testing.T) {
	r := newRig(t, true, 500)
	r.s.cfg.found = net.IPv4(127, 0, 0, 2)
	r.s.cfg.Port = freePort(t)
	_, err := r.s.Test()
	if err == nil {
		t.Skip("127.0.0.2 reached something here")
	}
	if !r.s.Status().TestFailed {
		t.Error("the page must say so")
	}
}

func TestLogHoldsNoAddresses(t *testing.T) {
	r := newRig(t, true, 500)
	r.s.Test()
	r.s.Close()
	data, _ := os.ReadFile(filepath.Join(r.dir, "log.txt"))
	if strings.Contains(string(data), "127.0.0.1") {
		t.Error(string(data))
	}
}

func TestGamesAtOnceAndWhatToForward(t *testing.T) {
	for chosen, want := range map[int]int{0: DefaultGames, -3: DefaultGames, 1: 1, 4: 4, MaxGames: MaxGames, 99: MaxGames} {
		if got := GamesAtOnce(chosen); got != want {
			t.Errorf("GamesAtOnce(%d) = %d, want %d", chosen, got, want)
		}
	}
	for games, last := range map[int]int{0: 3482, 2: 3482, 4: 3486, 16: 3510, 40: 3510} {
		if first, got := Forward(3478, games); first != 3478 || got != last {
			t.Errorf("Forward(3478, %d) = %d to %d, want 3478 to %d", games, first, got, last)
		}
	}
}

// Each relayed game takes its ports right after the relay's own, as many as the owner's number allows,
// and a port past that number is never opened. A higher number lets the next game in at once.
func TestGamesOnlyOnTheirPorts(t *testing.T) {
	r := newRig(t, true, 500)
	port := r.s.cfg.Port
	inRange := func(alloc net.PacketConn, games int) {
		t.Helper()
		got := alloc.LocalAddr().(*net.UDPAddr).Port
		if got <= port || got > port+portsPerGame*games {
			t.Errorf("relayed on %d, outside %d to %d", got, port+1, port+portsPerGame*games)
		}
	}
	for i := range portsPerGame * DefaultGames {
		creds, _ := r.s.Credentials("game" + strconv.Itoa(i/portsPerGame))
		alloc, err := r.client(t, creds).Allocate()
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		t.Cleanup(func() { alloc.Close() })
		inRange(alloc, DefaultGames)
	}
	past, _ := r.s.Credentials("game-past")
	if alloc, err := r.client(t, past).Allocate(); err == nil {
		alloc.Close()
		t.Fatal("a connection past the owner's number of games")
	}
	for p := port + portsPerGame*DefaultGames + 1; p <= port+portsPerGame*MaxGames; p++ {
		c, err := net.ListenPacket("udp", ":"+strconv.Itoa(p))
		if err != nil {
			t.Errorf("port %d is open past the owner's number", p)
			continue
		}
		c.Close()
	}
	r.store.Update(func(st *store.State) error { st.Settings.RelayGames = 3; return nil })
	alloc, err := r.client(t, past).Allocate()
	if err != nil {
		t.Fatalf("a third game once the owner allows three: %v", err)
	}
	defer alloc.Close()
	inRange(alloc, 3)
	if s := r.s.Status(); s.GamesAtOnce != 3 || s.FirstPort != port || s.LastPort != port+6 {
		t.Errorf("the page would say %+v", s)
	}
}

// A relayed game on the games' ports: each console has its own connection, and packets go both ways
// between the two, counted.
func TestRelayedGameOnTheGamesPorts(t *testing.T) {
	r := newRig(t, true, 500)
	host, _ := r.s.Credentials("game1")
	friend, _ := r.s.Credentials("game1")
	a, err := r.client(t, host).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := r.client(t, friend).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, alloc := range []net.PacketConn{a, b} {
		p := alloc.LocalAddr().(*net.UDPAddr).Port
		if p <= r.s.cfg.Port || p > r.s.cfg.Port+portsPerGame*DefaultGames {
			t.Errorf("relayed on %d", p)
		}
	}
	// A connection takes packets only from a peer it has written to, which gives it a permission, so
	// each console knocks first, as ICE's checks do.
	for _, knock := range [][2]net.PacketConn{{a, b}, {b, a}} {
		if _, err := knock[0].WriteTo([]byte("knock"), knock[1].LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	exchange := func(from, to net.PacketConn, payload string) {
		t.Helper()
		buf := make([]byte, 1500)
		for range 20 {
			if _, err := from.WriteTo([]byte(payload), to.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			to.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := to.ReadFrom(buf)
			if err == nil && string(buf[:n]) == payload {
				if addr.String() != from.LocalAddr().String() {
					t.Errorf("came from %v, want %v", addr, from.LocalAddr())
				}
				return
			}
		}
		t.Fatalf("%q never arrived", payload)
	}
	exchange(b, a, "hello")
	exchange(a, b, "picture")
	exchange(b, a, "buttons")
	if r.s.Status().UsedBytes == 0 {
		t.Error("a relayed game is counted")
	}
}

// fakeStun answers binding requests as a STUN server does, saying the asker's packets come from report.
func fakeStun(t *testing.T, network, listen string, report net.IP) string {
	t.Helper()
	conn, err := net.ListenPacket(network, listen)
	if err != nil {
		t.Skipf("no %s here: %v", network, err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			request := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if request.Decode() != nil || request.Type != stun.BindingRequest {
				continue
			}
			answer, err := stun.Build(stun.NewTransactionIDSetter(request.TransactionID), stun.BindingSuccess,
				&stun.XORMappedAddress{IP: report, Port: 40000}, stun.Fingerprint)
			if err == nil {
				conn.WriteTo(answer.Raw, from)
			}
		}
	}()
	return conn.LocalAddr().String()
}

// addressRig is a relay that has to find its own address, from a public address and a STUN list.
func addressRig(t *testing.T, allowPrivate bool, public string, stunList ...string) *Service {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, func(state *store.State) error {
		state.RelaySecret = []byte("0123456789abcdef0123456789abcdef")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(state *store.State) error {
		state.Settings.Address = public
		state.Settings.Stun = nil
		for i, a := range stunList {
			state.Settings.Stun = append(state.Settings.Stun, store.Stun{ID: strconv.Itoa(i), Address: a})
		}
		return nil
	})
	log, err := logfile.Open(filepath.Join(dir, "log.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Port: 3478, AllowPrivate: allowPrivate}, st, log)
}

// Behind a reverse proxy or Cloudflare, the public address leads to the proxy. The relay hands out
// where STUN sees its own packets come from instead, whatever the public address says.
func TestAddressIsWhereStunSeesThePackets(t *testing.T) {
	behindProxy := fakeStun(t, "udp4", "127.0.0.1:0", net.IPv4(8, 8, 4, 4))
	for _, public := range []string{"1.1.1.1:443", "https://1.1.1.1", "8.8.8.8:7845", ""} {
		s := addressRig(t, false, public, behindProxy)
		s.findAddress()
		if got := s.Status().Found; got != "8.8.4.4:3478" {
			t.Errorf("public address %q: relay at %q, want where STUN sees it", public, got)
		}
	}
}

// With no STUN answer there is no address, and the relay says it is off, rather than hand out
// whatever the public address leads to.
func TestNoStunAnswerIsNoAddress(t *testing.T) {
	private := fakeStun(t, "udp4", "127.0.0.1:0", net.IPv4(192, 168, 1, 50))
	s := addressRig(t, false, "1.1.1.1:443", "127.0.0.1:"+strconv.Itoa(freePort(t)), private)
	s.findAddress()
	if got := s.Status().Found; got != "" {
		t.Errorf("relay at %q with nothing public from STUN", got)
	}
	data, _ := os.ReadFile(filepath.Join(s.store.Dir(), "log.txt"))
	if !strings.Contains(string(data), "relay") {
		t.Errorf("the log says nothing: %s", data)
	}
}

// A server with no IPv4 asks over IPv6.
func TestAddressOverIPv6(t *testing.T) {
	v6 := fakeStun(t, "udp6", "[::1]:0", net.ParseIP("2606:4700:4700::1111"))
	s := addressRig(t, false, "", v6)
	s.findAddress()
	if got := s.Status().Found; got != "[2606:4700:4700::1111]:3478" {
		t.Errorf("relay at %q", got)
	}
}

// A test server for consoles on one home network keeps its public address's own name: there it is
// the address those consoles reach.
func TestTestServerTakesItsPublicAddress(t *testing.T) {
	outside := fakeStun(t, "udp4", "127.0.0.1:0", net.IPv4(8, 8, 4, 4))
	s := addressRig(t, true, "127.0.0.1:7845", outside)
	s.findAddress()
	if got := s.Status().Found; got != "127.0.0.1:3478" {
		t.Errorf("relay at %q", got)
	}
}
