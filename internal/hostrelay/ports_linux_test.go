//go:build linux

package hostrelay

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestMain doubles as a listener process for the tests below: with
// HOSTRELAY_TEST_LISTEN set it listens on 127.0.0.1, prints the port and
// waits to be killed.
func TestMain(m *testing.M) {
	if os.Getenv("HOSTRELAY_TEST_LISTEN") != "" {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(ln.Addr().(*net.TCPAddr).Port)
		select {}
	}
	os.Exit(m.Run())
}

func parseProcNetFor(t *testing.T, table string, v6 bool) ([]string, error) {
	t.Helper()
	rows, err := parseProcNet(strings.NewReader(table), v6)
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s #%d", r.addr, r.inode))
	}
	return out, err
}

// TestParseProcNet: /proc/net/tcp{,6} rows are read in host byte order, and
// only the LISTEN ones count.
func TestParseProcNet(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the table below is a little-endian host's")
	}
	v4 := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0277 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1002 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1F90 0100007F:D3A2 01 00000000:00000000 00:00000000 00000000  1000        0 1003 1 0000000000000000 20 4 30 10 -1
`
	v6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:0277 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2001 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2002 1 0000000000000000 100 0 0 10 0
`
	got4, err := parseProcNetFor(t, v4, false)
	if err != nil {
		t.Fatal(err)
	}
	got6, err := parseProcNetFor(t, v6, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1:631 #1001", "0.0.0.0:8080 #1002", "[::1]:631 #2001", "[::]:5432 #2002"}
	if all := append(got4, got6...); !slices.Equal(all, want) {
		t.Fatalf("got  %q\nwant %q", all, want)
	}
}

// startListener starts a process of its own listening on 127.0.0.1 and
// returns its port.
func startListener(t *testing.T) uint16 {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "HOSTRELAY_TEST_LISTEN=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var port uint16
	if _, err := fmt.Sscan(line, &port); err != nil {
		t.Fatalf("listener said %q", line)
	}
	return port
}

func hasPort(ports []Port, p uint16) bool {
	return slices.ContainsFunc(ports, func(x Port) bool { return x.Port == p })
}

// TestScanListsTheHostsListeners: a listener of another process is listed,
// dialled at 127.0.0.1; this process's own, and one of a process named as
// fleet's (Self), are not.
func TestScanListsTheHostsListeners(t *testing.T) {
	own, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ownPort := uint16(own.Addr().(*net.TCPAddr).Port)
	other := startListener(t)

	s := &Scanner{Self: []string{"not-this-test"}}
	ports, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ports, Port{Port: other, Dial: ap(fmt.Sprintf("127.0.0.1:%d", other))}) {
		t.Fatalf("another process's listener %d is missing from %v", other, ports)
	}
	if hasPort(ports, ownPort) {
		t.Fatalf("this process's own listener %d is listed", ownPort)
	}

	// The same listener, its process now named as fleet's: the scanner
	// remembers sockets it judged, so a fresh one judges again.
	name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	fleetish := &Scanner{Self: []string{strings.TrimSpace(string(name))}}
	ports, err = fleetish.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasPort(ports, other) {
		t.Fatalf("a fleet process's listener %d is listed", other)
	}

}
