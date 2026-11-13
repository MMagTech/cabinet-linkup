package relay

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

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
	netip.MustParsePrefix("3fff::/20"),
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
