package hostrelay

import (
	"bufio"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// lsof_parse.go reads lsof's listing of the listening sockets: the daemon
// host's ports on macOS and the BSDs (it builds everywhere, so its tests run
// on Linux too).

// parseLsof reads lsof's field output (-F pctn): a process's p and c lines,
// then each of its files' f, t and n lines.
func parseLsof(r io.Reader, own func(pid int, command string) bool) ([]Listener, error) {
	var out []Listener
	pid, command, typ := 0, "", ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
			command, typ = "", ""
		case 'c':
			command = v
		case 'f':
			typ = ""
		case 't':
			typ = v
		case 'n':
			addr, ok := parseLsofName(v, typ)
			if ok {
				out = append(out, Listener{Addr: addr, Own: own(pid, command)})
			}
		}
	}
	return out, sc.Err()
}

// parseLsofName reads a listening socket's name: "*:8080", "127.0.0.1:631",
// "[::1]:631"; "*" is every interface of the socket's family (typ, "IPv4"
// or "IPv6").
func parseLsofName(name, typ string) (netip.AddrPort, bool) {
	i := strings.LastIndexByte(name, ':')
	if i < 0 {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(name[i+1:], 10, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	host := strings.TrimSuffix(strings.TrimPrefix(name[:i], "["), "]")
	var addr netip.Addr
	switch {
	case host == "*" && typ == "IPv6":
		addr = netip.IPv6Unspecified()
	case host == "*":
		addr = netip.IPv4Unspecified()
	default:
		if addr, err = netip.ParseAddr(host); err != nil {
			return netip.AddrPort{}, false
		}
	}
	return netip.AddrPortFrom(addr, uint16(port)), true
}
