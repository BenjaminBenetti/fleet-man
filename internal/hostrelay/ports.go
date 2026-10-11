package hostrelay

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Port is a TCP port listening on the daemon host and the address fleetd
// dials it at.
type Port struct {
	Port uint16
	Dial netip.AddrPort
}

// Listener is one listening TCP socket on the daemon host, as the OS lists
// it.
type Listener struct {
	Addr netip.AddrPort
	// Own marks fleet's own listener (fleetd's, or a fleet client's: a port
	// forward, a published port).
	Own bool
}

// Scanner lists the daemon host's listening TCP ports (/proc/net/tcp and
// tcp6 on Linux, lsof elsewhere), leaving out fleet's own listeners: a pod
// that reached one of those through host-gateway would loop back into fleet
// (an instance's own forwarded port), and the mirror would hold the port in
// the pod against the instance's own service. The zero value is ready; it
// remembers which sockets it found to be fleet's.
type Scanner struct {
	// Self are the process names (comm on Linux, lsof's command elsewhere)
	// whose listeners are fleet's own; nil is "fleet" and this executable's
	// own name. This process's listeners always are.
	Self []string

	mu   sync.Mutex
	own  map[uint64]bool
	list func(ctx context.Context) ([]Listener, error)
}

// Scan is the listening ports that are not fleet's own, by port, each with
// the address to dial it at.
func (s *Scanner) Scan(ctx context.Context) ([]Port, error) {
	list := s.list
	if list == nil {
		list = s.listeners
	}
	ls, err := list(ctx)
	if err != nil {
		return nil, err
	}
	return Choose(ls), nil
}

// names is the process names whose listeners are fleet's own.
func (s *Scanner) names() []string {
	if s.Self != nil {
		return s.Self
	}
	names := []string{"fleet"}
	if exe, err := os.Executable(); err == nil {
		names = append(names, filepath.Base(exe))
	}
	return names
}

// isSelf reports whether a process named name (comm, truncated to 15 bytes
// on Linux) is fleet's.
func (s *Scanner) isSelf(name string) bool {
	for _, n := range s.names() {
		if name == n || (len(name) == 15 && len(n) > 15 && n[:15] == name) {
			return true
		}
	}
	return false
}

// rank orders the addresses one port listens at by how fleetd dials it:
// every interface or 127.0.0.1 (dialled at 127.0.0.1) first, then other
// IPv4 loopbacks, IPv6 (dialled at ::1 for every interface), and a port
// bound to one other address only at that address. A link-local IPv6
// address, which needs its zone, is not dialled.
func rank(a netip.Addr) (netip.Addr, int, bool) {
	a = a.Unmap()
	loop4 := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	switch {
	case a.Is4() && a.IsUnspecified():
		return loop4, 0, true
	case a == loop4:
		return loop4, 1, true
	case a.Is4() && a.IsLoopback():
		return a, 2, true
	case a.Is6() && a.IsUnspecified():
		return netip.IPv6Loopback(), 3, true
	case a == netip.IPv6Loopback():
		return a, 4, true
	case a.Is4():
		return a, 5, true
	case a.IsLinkLocalUnicast() || a.Zone() != "":
		return netip.Addr{}, 0, false
	}
	return a, 6, true
}

// Choose is the ports listeners listen at, leaving out fleet's own, each
// with the address it is dialled at, sorted by port.
func Choose(listeners []Listener) []Port {
	best := map[uint16]int{}
	dial := map[uint16]netip.Addr{}
	own := map[uint16]bool{}
	for _, l := range listeners {
		p := l.Addr.Port()
		if p == 0 {
			continue
		}
		if l.Own {
			own[p] = true
			continue
		}
		addr, r, ok := rank(l.Addr.Addr())
		if !ok {
			continue
		}
		if cur, seen := best[p]; !seen || r < cur {
			best[p], dial[p] = r, addr
		}
	}
	out := make([]Port, 0, len(dial))
	for p, a := range dial {
		// A port fleet itself listens at is fleet's, whatever else shares it.
		if own[p] {
			continue
		}
		out = append(out, Port{Port: p, Dial: netip.AddrPortFrom(a, p)})
	}
	slices.SortFunc(out, func(a, b Port) int { return int(a.Port) - int(b.Port) })
	return out
}
