package sshtunnel

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDiscovery(t *testing.T) {
	d, err := parseDiscovery("motd noise\nFLEET_OK 41234 tok-abc\n")
	if err != nil || d != (Discovery{Port: 41234, Token: "tok-abc"}) {
		t.Fatalf("ok line: %+v, %v", d, err)
	}
	if _, err := parseDiscovery("FLEET_SSH_OFF\n"); !errors.Is(err, ErrSSHModeOff) {
		t.Fatalf("ssh off: %v", err)
	}
	if _, err := parseDiscovery("FLEET_NO_DAEMON\n"); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("no daemon: %v", err)
	}
	for _, bad := range []string{"", "garbage", "FLEET_OK 41234", "FLEET_OK notaport tok", "FLEET_OK 70000 tok"} {
		if _, err := parseDiscovery(bad); err == nil {
			t.Errorf("parseDiscovery(%q) should fail", bad)
		}
	}
}

// fakeFleet writes a stand-in `fleet` executable into a fresh directory and
// returns that directory, to be put FIRST on the probe's PATH. The real fleet
// (this devcontainer ships one in /usr/bin) must never be reached from a test:
// `fleet list` would auto-spawn a real daemon under the temp HOME.
func fakeFleet(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "fleet"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runScript executes discoverScript with sh against an isolated HOME, with
// fakeBin (a fakeFleet dir) shadowing any real fleet, returning stdout.
func runScript(t *testing.T, home, fakeBin string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	cmd := exec.Command(sh)
	cmd.Stdin = strings.NewReader(discoverScript)
	cmd.Env = []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, errb.String())
	}
	return out.String()
}

// TestDiscoverScript runs the real remote-side probe under sh across its three
// outcomes, so the shell and the Go parser are proven against each other.
func TestDiscoverScript(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Nothing there, and the client command fails (fleet can't start a daemon):
	// no daemon — and no wait loop, since nothing was started.
	broken := fakeFleet(t, "exit 1\n")
	if _, err := parseDiscovery(runScript(t, home, broken)); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("empty ~/.fleet: want ErrNoDaemon, got %v", err)
	}

	// A running daemon (server.version) without the SSH listener: mode off; the
	// script must not even try to spawn (a fake that would "succeed" is unused).
	if err := os.WriteFile(filepath.Join(dir, "server.version"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDiscovery(runScript(t, home, broken)); !errors.Is(err, ErrSSHModeOff) {
		t.Fatalf("version but no port: want ErrSSHModeOff, got %v", err)
	}

	// Listener up: port + token come back.
	if err := os.WriteFile(filepath.Join(dir, "ssh.port"), []byte("41234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.token"), []byte("tok-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := parseDiscovery(runScript(t, home, broken))
	if err != nil || d != (Discovery{Port: 41234, Token: "tok-abc"}) {
		t.Fatalf("listener up: %+v, %v", d, err)
	}
}

// TestDiscoverScriptStartsDaemon covers the auto-spawn branch: with no daemon
// files present, the script finds `fleet` on PATH, runs it, and waits for the
// files it writes. The fake fleet stands in for the daemon's startup by
// writing ssh.port + mcp.token + server.version itself.
func TestDiscoverScriptStartsDaemon(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := fakeFleet(t,
		"[ \"$1\" = list ] || exit 9\n"+ // the script must call a cheap client command
			"printf 40001 > \"$HOME/.fleet/ssh.port\"\n"+
			"printf tok-spawned > \"$HOME/.fleet/mcp.token\"\n"+
			"printf v1 > \"$HOME/.fleet/server.version\"\n")
	d, err := parseDiscovery(runScript(t, home, bin))
	if err != nil || d != (Discovery{Port: 40001, Token: "tok-spawned"}) {
		t.Fatalf("spawn path: %+v, %v", d, err)
	}
}

func TestLastLine(t *testing.T) {
	if got := lastLine("Warning: x\nben@host: Permission denied (publickey).\n\n"); got != "ben@host: Permission denied (publickey)." {
		t.Fatalf("lastLine = %q", got)
	}
	if got := lastLine("\n  \n"); got != "" {
		t.Fatalf("lastLine(blank) = %q", got)
	}
}
