package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/turn/v4"

	"github.com/MMagTech/cabinet-linkup/internal/logfile"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

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
