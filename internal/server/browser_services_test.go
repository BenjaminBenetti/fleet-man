package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// The in-instance browser servers' scripts run here against a "container"
// that is this host: the pid and log files live in a temp dir, and the fleet
// binary is a stand-in that records its arguments and, as the real one does,
// prints its listening line and serves until signalled.

// localBrowserBackend runs ExecCommand on this host and stages the fleet
// binary as a fleetlaunch.Stager whose refresh the test decides.
type localBrowserBackend struct {
	backend.Backend
	refresh bool
	staged  int
	scripts []string
}

func (l *localBrowserBackend) ExecCommand(_ string, argv []string) *backend.Cmd {
	l.scripts = append(l.scripts, strings.Join(argv, " "))
	if strings.Contains(strings.Join(argv, " "), "privoxy --pidfile") {
		// Never install a package on the test host.
		return backend.NewCmd(exec.Command("echo", "ALREADY_RUNNING"), nil)
	}
	return backend.NewCmd(exec.Command(argv[0], argv[1:]...), nil)
}

func (l *localBrowserBackend) StageFleet(_ string, beforeRefresh func() error) (bool, error) {
	l.staged++
	if !l.refresh {
		return false, nil
	}
	l.refresh = false
	if beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// loopbackBrowserBackend is one the daemon dials the instance's loopback on.
type loopbackBrowserBackend struct{ *localBrowserBackend }

func (loopbackBrowserBackend) DialPort(context.Context, string, int) (io.ReadWriteCloser, error) {
	return nil, os.ErrInvalid
}

// fakeFleet is the stand-in fleet binary: "browser-proxy" and "landing-page"
// print their listening line and serve until signalled; $FAKE_PROXY_FAIL
// makes the proxy fail as a busy port would.
const fakeFleet = `#!/bin/sh
echo "$*" >> "$(dirname "$0")/args.log"
case "$1" in
browser-proxy)
  if [ -f "$(dirname "$0")/fail" ]; then echo "listen tcp :58888: bind: address already in use" >&2; exit 1; fi
  echo "fleet browser-proxy: listening on [::]:58888" >&2 ;;
landing-page) echo "fleet landing-page: listening on [::]:16767" >&2 ;;
*) exit 3 ;;
esac
trap 'kill "$c" 2>/dev/null; exit 0' TERM
sleep 120 & c=$!
wait
`

// browserRig points the servers' files at a temp dir.
func browserRig(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the scripts' liveness check reads /proc")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fleet")
	if err := os.WriteFile(bin, []byte(fakeFleet), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := browserPaths
	browserPaths = browserFiles{
		proxyPid: filepath.Join(dir, "proxy.pid"), proxyLog: filepath.Join(dir, "proxy.log"),
		landingPid: filepath.Join(dir, "landing.pid"), landingLog: filepath.Join(dir, "landing.log"),
		legacyPid: filepath.Join(dir, "privoxy.pid"), bin: bin,
	}
	t.Cleanup(func() {
		for _, p := range []string{browserPaths.proxyPid, browserPaths.landingPid, browserPaths.legacyPid} {
			if pid := readPid(p); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		browserPaths = orig
	})
	return dir
}

func readPid(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func launches(t *testing.T, dir string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// On Docker the built-in proxy listens on every interface (the daemon dials
// the container's address), replaces an older fleet's privoxy on its port,
// and a second open reuses it.
func TestBrowserProxyStartsOnceAndReplacesPrivoxy(t *testing.T) {
	dir := browserRig(t)
	privoxy := exec.Command("sleep", "120")
	if err := privoxy.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = privoxy.Wait() }()
	if err := os.WriteFile(browserPaths.legacyPid, []byte(strconv.Itoa(privoxy.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || got[0] != "browser-proxy --port 58888" {
		t.Fatalf("launches %q", got)
	}
	first := readPid(browserPaths.proxyPid)
	if !alive(first) {
		t.Fatal("the proxy is not running")
	}
	if _, err := os.Stat(browserPaths.legacyPid); !os.IsNotExist(err) {
		t.Fatalf("the privoxy pid file should be gone: %v", err)
	}
	if err := privoxy.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("the old privoxy should have been stopped")
	}

	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || readPid(browserPaths.proxyPid) != first {
		t.Fatalf("a second open should reuse the running proxy: %q", got)
	}
	if b.staged != 2 {
		t.Fatalf("each open stages the binary when stale: %d", b.staged)
	}
	for _, s := range b.scripts {
		if strings.Contains(s, "privoxy --pidfile") || strings.Contains(s, "apt-get") {
			t.Fatalf("an instance that runs fleet must not install privoxy: %s", s)
		}
	}
}

// Where the daemon dials the instance's own loopback (Kubernetes), both
// servers bind 127.0.0.1.
func TestBrowserServicesBindLoopbackOnAPortDialer(t *testing.T) {
	dir := browserRig(t)
	b := loopbackBrowserBackend{&localBrowserBackend{}}
	if err := ensureBrowserServices(b, "/ws", true); err != nil {
		t.Fatal(err)
	}
	got := launches(t, dir)
	want := []string{"browser-proxy --port 58888 --addr 127.0.0.1", "landing-page --port 16767 --workspace . --addr 127.0.0.1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("launches %q, want %q", got, want)
	}
	if !alive(readPid(browserPaths.proxyPid)) || !alive(readPid(browserPaths.landingPid)) {
		t.Fatal("both servers should run")
	}
}

// A refresh of the staged binary restarts both servers on it, the landing
// page too when this open does not ask for it.
func TestBrowserServicesRestartAfterARefresh(t *testing.T) {
	dir := browserRig(t)
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", true); err != nil {
		t.Fatal(err)
	}
	proxy, landing := readPid(browserPaths.proxyPid), readPid(browserPaths.landingPid)
	b.refresh = true
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if alive(proxy) || alive(landing) {
		t.Fatal("the old binary's servers should be stopped before the refresh")
	}
	if p, l := readPid(browserPaths.proxyPid), readPid(browserPaths.landingPid); !alive(p) || !alive(l) || p == proxy || l == landing {
		t.Fatalf("both should run again on the new binary: proxy %d->%d landing %d->%d", proxy, p, landing, l)
	}
	if n := len(launches(t, dir)); n != 4 {
		t.Fatalf("launches: %d: %q", n, launches(t, dir))
	}
}

// A pid file left by a restart, naming a process that is not the proxy,
// does not count as the proxy running.
func TestBrowserServicesIgnoreAReusedPid(t *testing.T) {
	dir := browserRig(t)
	other := exec.Command("sleep", "120")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	if err := os.WriteFile(browserPaths.proxyPid, []byte(strconv.Itoa(other.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBrowserServices(&localBrowserBackend{}, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || !alive(readPid(browserPaths.proxyPid)) || readPid(browserPaths.proxyPid) == other.Process.Pid {
		t.Fatalf("the proxy should have been started: %q", got)
	}
}

func TestBrowserProxyStartFailureSaysWhy(t *testing.T) {
	dir := browserRig(t)
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := ensureBrowserServices(&localBrowserBackend{}, "/ws", false)
	if err == nil || !strings.Contains(err.Error(), "browser proxy") || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("a failed start should name the proxy and its log: %v", err)
	}
}

// An instance that cannot run the daemon's fleet binary keeps privoxy.
func TestBrowserProxyFallsBackToPrivoxyWhereFleetCannotRun(t *testing.T) {
	dir := browserRig(t)
	orig := builtinBrowserProxy
	builtinBrowserProxy = func(backend.Backend) bool { return false }
	defer func() { builtinBrowserProxy = orig }()
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if len(b.scripts) != 1 || !strings.Contains(b.scripts[0], "privoxy --pidfile") {
		t.Fatalf("the privoxy setup should run: %q", b.scripts)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.log")); !os.IsNotExist(err) {
		t.Fatal("the built-in proxy must not start there")
	}
}

func TestBuiltinBrowserProxyRule(t *testing.T) {
	if !builtinBrowserProxy(&localBrowserBackend{}) {
		t.Error("a backend that stages its own fleet build runs the built-in proxy")
	}
	type plain struct{ backend.Backend }
	if got := builtinBrowserProxy(plain{}); got != (runtime.GOOS == "linux") {
		t.Errorf("a backend that gets the daemon's binary: %v on %s", got, runtime.GOOS)
	}
	if browserBindAddr(plain{}) != "" || browserBindAddr(loopbackBrowserBackend{}) != "127.0.0.1" {
		t.Error("only a PortDialer's instance binds loopback")
	}
}
