//go:build linux

package hostrelay

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is where the listing reads /proc (a var for tests).
var procRoot = "/proc"

// tcpListen is the LISTEN state in /proc/net/tcp.
const tcpListen = "0A"

// procListener is one LISTEN row of /proc/net/tcp{,6}.
type procListener struct {
	addr  netip.AddrPort
	inode uint64
}

// listeners lists the listening TCP sockets of this network namespace, each
// marked when its socket belongs to a fleet process.
func (s *Scanner) listeners(ctx context.Context) ([]Listener, error) {
	var rows []procListener
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(procRoot, "net", name))
		if errors.Is(err, fs.ErrNotExist) && name == "tcp6" {
			// No IPv6 in this kernel.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list the listening ports: %w", err)
		}
		got, err := parseProcNet(f, name == "tcp6")
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("list the listening ports: %s: %w", name, err)
		}
		rows = append(rows, got...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	own := s.ownInodes(rows)
	out := make([]Listener, 0, len(rows))
	for _, r := range rows {
		out = append(out, Listener{Addr: r.addr, Own: own[r.inode]})
	}
	return out, nil
}

// parseProcNet reads the LISTEN rows of a /proc/net/tcp (or tcp6) table.
func parseProcNet(r io.Reader, v6 bool) ([]procListener, error) {
	var out []procListener
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[3] != tcpListen {
			continue
		}
		addr, err := parseProcAddr(f[1], v6)
		if err != nil {
			return nil, err
		}
		inode, _ := strconv.ParseUint(f[9], 10, 64)
		out = append(out, procListener{addr: addr, inode: inode})
	}
	return out, sc.Err()
}

// parseProcAddr reads a /proc/net/tcp address: the IP as 32-bit words in
// host byte order, hex, then ":" and the port, hex.
func parseProcAddr(s string, v6 bool) (netip.AddrPort, error) {
	ipHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("address %q", s)
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("address %q: %w", s, err)
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) || (len(raw) == 16) != v6 {
		return netip.AddrPort{}, fmt.Errorf("address %q", s)
	}
	// Each word was printed as a number: its bytes are the native order's.
	ip := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(ip[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// ownInodes is which of the rows' sockets belong to a fleet process: this
// one's, or a process of this user named as Self says. The answers are kept
// per socket, so the processes are walked only when a socket shows that was
// not seen before.
func (s *Scanner) ownInodes(rows []procListener) map[uint64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.own == nil {
		s.own = map[uint64]bool{}
	}
	live := make(map[uint64]bool, len(rows))
	unknown := false
	for _, r := range rows {
		live[r.inode] = true
		if _, ok := s.own[r.inode]; !ok {
			unknown = true
		}
	}
	for ino := range s.own {
		if !live[ino] {
			delete(s.own, ino)
		}
	}
	if !unknown {
		return s.own
	}
	owners := socketOwners(live)
	for _, r := range rows {
		if _, ok := s.own[r.inode]; ok {
			continue
		}
		pid, found := owners[r.inode]
		s.own[r.inode] = found && (pid == os.Getpid() || s.isSelf(processName(pid)))
	}
	return s.own
}

// socketOwners maps each of the wanted socket inodes to the process holding
// it, as far as /proc lets this process see (its own user's processes).
func socketOwners(want map[uint64]bool) map[uint64]int {
	out := map[uint64]int{}
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join(procRoot, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			ino, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
			if err == nil && want[ino] {
				if _, seen := out[ino]; !seen {
					out[ino] = pid
				}
			}
		}
		if len(out) == len(want) {
			break
		}
	}
	return out
}

// processName is pid's comm ("" when it cannot be read).
func processName(pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
