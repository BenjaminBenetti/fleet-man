package hostrelay

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// TestChooseDialsEachPortWhereItListens: a port is dialled at 127.0.0.1 when
// it listens on every interface or there, at ::1 for IPv6 only, and at its
// own address when bound to one other address only; a link-local address is
// not dialled; a port any fleet process listens at is left out whatever
// else shares it.
func TestChooseDialsEachPortWhereItListens(t *testing.T) {
	got := Choose([]Listener{
		{Addr: ap("[::]:8080")},
		{Addr: ap("0.0.0.0:8080")},
		{Addr: ap("127.0.0.1:631")},
		{Addr: ap("[::1]:631")},
		{Addr: ap("[::]:5432")},
		{Addr: ap("[::1]:9229")},
		{Addr: ap("127.0.0.53:53")},
		{Addr: ap("192.168.1.5:3000")},
		{Addr: ap("[fe80::1%eth0]:4000")},
		{Addr: ap("[::ffff:127.0.0.1]:7000")},
		{Addr: ap("127.0.0.1:17380"), Own: true},
		{Addr: ap("192.168.1.5:17380")},
		{Addr: ap("0.0.0.0:0")},
	})
	want := []Port{
		{53, ap("127.0.0.53:53")},
		{631, ap("127.0.0.1:631")},
		{3000, ap("192.168.1.5:3000")},
		{5432, ap("[::1]:5432")},
		{7000, ap("127.0.0.1:7000")},
		{8080, ap("127.0.0.1:8080")},
		{9229, ap("[::1]:9229")},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// TestParseLsof: lsof's field listing (macOS, the BSDs) gives each listening
// socket with its process; "*" is the socket's own family's every interface.
func TestParseLsof(t *testing.T) {
	out := "p101\ncpostgres\nf7\ntIPv6\nn*:5432\nf8\ntIPv4\nn*:5432\np202\ncfleet\nf9\ntIPv4\nn127.0.0.1:17380\np303\ncControlCe\nf10\ntIPv6\nn[::1]:7000\nf11\ntIPv4\nn127.0.0.1:7000\nf12\ntIPv4\nnbogus\n"
	got, err := parseLsof(strings.NewReader(out), func(pid int, command string) bool { return command == "fleet" })
	if err != nil {
		t.Fatal(err)
	}
	want := []Listener{
		{Addr: ap("[::]:5432")}, {Addr: ap("0.0.0.0:5432")},
		{Addr: ap("127.0.0.1:17380"), Own: true},
		{Addr: ap("[::1]:7000")}, {Addr: ap("127.0.0.1:7000")},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// TestScannerScans: a Scanner's listing goes through Choose.
func TestScannerScans(t *testing.T) {
	s := &Scanner{list: func(context.Context) ([]Listener, error) {
		return []Listener{{Addr: ap("0.0.0.0:9000")}, {Addr: ap("127.0.0.1:9001"), Own: true}}, nil
	}}
	got, err := s.Scan(context.Background())
	if err != nil || !slices.Equal(got, []Port{{9000, ap("127.0.0.1:9000")}}) {
		t.Fatalf("%v %v", got, err)
	}
}

// TestIsSelfMatchesTruncatedNames: Linux keeps 15 bytes of a process name.
func TestIsSelfMatchesTruncatedNames(t *testing.T) {
	s := &Scanner{Self: []string{"fleet", "fleet-under-test-build"}}
	for name, want := range map[string]bool{"fleet": true, "fleet-under-tes": true, "fleetx": false, "python3": false, "": false} {
		if got := s.isSelf(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}
