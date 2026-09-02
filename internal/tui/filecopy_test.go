package tui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

func TestResolveLocalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"absolute used as-is", "/abs/target", "/abs/target"},
		{"bare tilde is home", "~", home},
		{"tilde-slash resolves against home", "~/builds/tool", filepath.Join(home, "builds/tool")},
		{"relative resolves against home", "builds/tool", filepath.Join(home, "builds/tool")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLocalPath(tc.path); got != tc.want {
				t.Errorf("resolveLocalPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestTUILocalPolicyResolveDest covers the human-machine dest policy: empty goes
// to the downloads folder (no Downloads dir in the temp home, so home itself),
// a directory keeps the source name, and a file path is used as-is.
func TestTUILocalPolicyResolveDest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	policy := tuiLocalPolicy{}

	cases := []struct {
		name, dest, src, want string
	}{
		{"empty means downloads", "", "tool", filepath.Join(home, "tool")},
		{"trailing slash keeps name", "~/builds/", "tool", filepath.Join(home, "builds", "tool")},
		{"file path used as-is", "~/out.bin", "tool", filepath.Join(home, "out.bin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ResolveDest(tc.dest, tc.src)
			if err != nil {
				t.Fatalf("ResolveDest: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveDest(%q,%q) = %q, want %q", tc.dest, tc.src, got, tc.want)
			}
		})
	}
}

// TestResolveTUIEndpoint covers how a delegated copy resolves endpoints against
// the originating instance: self is the sender, a bare instance defaults to the
// sender's fleet, and a plain path is local to this (the TUI's) machine.
func TestResolveTUIEndpoint(t *testing.T) {
	cases := []struct {
		arg  string
		want fleetclient.ResolvedEndpoint
	}{
		{":build/out", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "self", Path: "build/out"}},
		{"other:/tmp/x", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "other", Path: "/tmp/x"}},
		{"two/other:/tmp/x", fleetclient.ResolvedEndpoint{Fleet: "two", Instance: "other", Path: "/tmp/x"}},
		// host: (and, defensively, a bare path) resolve to this machine's disk.
		{"host:report.csv", fleetclient.ResolvedEndpoint{Local: true, Path: "report.csv"}},
		{"host:/tmp/x", fleetclient.ResolvedEndpoint{Local: true, Path: "/tmp/x"}},
		{"report.csv", fleetclient.ResolvedEndpoint{Local: true, Path: "report.csv"}},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got := resolveTUIEndpoint(tc.arg, "myfleet", "self")
			if got != tc.want {
				t.Errorf("resolveTUIEndpoint(%q) = %+v, want %+v", tc.arg, got, tc.want)
			}
		})
	}
}

// TestOpenDeliveredRefusesNonLocal is the safety check behind `fleet open`: a
// crafted envelope with open=true and an instance destination must not make the
// TUI open a same-named path on the human's machine.
func TestOpenDeliveredRefusesNonLocal(t *testing.T) {
	err := openDelivered(fleetclient.ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/tmp/x"}, "/tmp/x")
	if err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Fatalf("non-local destination should be refused, got %v", err)
	}
}

// TestFileCopyDoneStatusLine covers the four outcomes a delegated copy/open
// reports on the status bar.
func TestFileCopyDoneStatusLine(t *testing.T) {
	cases := []struct {
		name string
		msg  fileCopyDoneMsg
		want string
	}{
		{"copy failed", fileCopyDoneMsg{src: ":a", err: errors.New("boom")}, "Copy of :a failed: boom"},
		{"copied", fileCopyDoneMsg{src: ":a", dest: "/h/a"}, "Copied :a -> /h/a"},
		{"opened", fileCopyDoneMsg{src: ":a", dest: "/h/a", opened: true}, "Opened :a (/h/a)"},
		{"open failed", fileCopyDoneMsg{src: ":a", dest: "/h/a", openErr: errors.New("no handler")}, "Copied :a -> /h/a, but could not open it: no handler"},
	}
	for _, tc := range cases {
		if got := tc.msg.statusLine(); got != tc.want {
			t.Errorf("%s: statusLine() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
