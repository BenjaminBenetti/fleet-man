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
	"sync"
	"syscall"
	"testing"
	"time"

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
	mu      sync.Mutex
	refresh bool
	staged  int
	scripts []string
}

func (l *localBrowserBackend) ExecCommand(_ string, argv []string) *backend.Cmd {
	l.mu.Lock()
	l.scripts = append(l.scripts, strings.Join(argv, " "))
	l.mu.Unlock()
	if strings.Contains(strings.Join(argv, " "), "privoxy --pidfile") {
		// Never install a package on the test host.
		return backend.NewCmd(exec.Command("echo", "ALREADY_RUNNING"), nil)
	}
	return backend.NewCmd(exec.Command(argv[0], argv[1:]...), nil)
}

func (l *localBrowserBackend) StageFleet(_ string, beforeRefresh func() error) (bool, error) {
	l.mu.Lock()
	l.staged++
	refresh := l.refresh
	l.refresh = false
	l.mu.Unlock()
	if !refresh {
		return false, nil
	}
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
  if [ -f "$(dirname "$0")/slow" ]; then sleep 2; fi
  echo "fleet browser-proxy: listening on [::]:58888" >&2 ;;
landing-page) echo "fleet landing-page: listening on [::]:16767" >&2 ;;
*) exit 3 ;;
esac
if [ -f "$(dirname "$0")/stubborn" ]; then
  # A server that does not exit on SIGTERM.
  trap '' TERM
  sleep 120 & c=$!
  while kill -0 "$c" 2>/dev/null; do wait "$c"; done
  exit 0
fi
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
	privoxy := exec.Command(fakePrivoxy(t, dir), "120")
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

// fakePrivoxy is a sleep whose command line names privoxy, as an older
// fleet's proxy's does.
func fakePrivoxy(t *testing.T, dir string) string {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	path := filepath.Join(dir, "privoxy")
	if err := os.Symlink(sleep, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// The privoxy pid file an older fleet left behind may outlive its privoxy
// (a container restart): the process that has its pid now is not stopped.
func TestBrowserProxyLeavesAStalePrivoxyPidAlone(t *testing.T) {
	dir := browserRig(t)
	other := exec.Command("sleep", "120")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	if err := os.WriteFile(browserPaths.legacyPid, []byte(strconv.Itoa(other.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBrowserServices(&localBrowserBackend{}, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if !alive(other.Process.Pid) || isZombie(other.Process.Pid) {
		t.Fatal("a process that is not privoxy was killed for having the old privoxy's pid")
	}
	if _, err := os.Stat(browserPaths.legacyPid); !os.IsNotExist(err) {
		t.Fatalf("the stale privoxy pid file should be gone: %v", err)
	}
	if got := launches(t, dir); len(got) != 1 || !alive(readPid(browserPaths.proxyPid)) {
		t.Fatalf("the proxy should run: %q", got)
	}
}

// Two opens of one instance at once start one proxy, and its pid file names
// it.
func TestBrowserServicesStartOncePerInstanceAtATime(t *testing.T) {
	dir := browserRig(t)
	b := &localBrowserBackend{}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ensureInstanceBrowserServices("alpha/a", b, "/ws", false)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := launches(t, dir); len(got) != 1 || !alive(readPid(browserPaths.proxyPid)) {
		t.Fatalf("one proxy should have started, and run: %q", got)
	}
	if len(browserStarts.locks) != 0 {
		t.Fatalf("the instance's lock should be gone once nobody holds it: %v", browserStarts.locks)
	}
}

// A start that fails (another process has the port) leaves the pid file of
// the server that runs alone.
func TestBrowserProxyFailedStartKeepsTheRunningPid(t *testing.T) {
	dir := browserRig(t)
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	first := readPid(browserPaths.proxyPid)
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := startBrowserServer(b, "/ws", browserProxyArgs(""), browserPaths.proxyPid, browserPaths.proxyLog, ""); err == nil {
		t.Fatal("the second start should fail")
	}
	if got := readPid(browserPaths.proxyPid); got != first || !alive(got) {
		t.Fatalf("the pid file should still name the running proxy %d: %d", first, got)
	}
}

// A server still starting when the wait ends is recorded, so the next open
// finds it rather than starting another on its port.
func TestBrowserProxySlowStartIsRecorded(t *testing.T) {
	dir := browserRig(t)
	orig := browserStartWait
	browserStartWait = 300 * time.Millisecond
	defer func() { browserStartWait = orig }()
	if err := os.WriteFile(filepath.Join(dir, "slow"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", false); err == nil {
		t.Fatal("a start that outlasts the wait should report it")
	}
	slow := readPid(browserPaths.proxyPid)
	if !alive(slow) {
		t.Fatalf("the slow proxy should be recorded and left running: %d", slow)
	}
	waitUntil(t, "the slow proxy to listen", func() bool {
		data, _ := os.ReadFile(browserPaths.proxyLog)
		return strings.Contains(string(data), "listening on")
	})
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || readPid(browserPaths.proxyPid) != slow {
		t.Fatalf("the next open should find the slow proxy: %q", got)
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

// A server that does not exit on SIGTERM is killed before the restart, so
// the new one gets the port.
func TestBrowserServicesKillAServerThatIgnoresSIGTERM(t *testing.T) {
	dir := browserRig(t)
	b := &localBrowserBackend{}
	if err := os.WriteFile(filepath.Join(dir, "stubborn"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBrowserServices(b, "/ws", true); err != nil {
		t.Fatal(err)
	}
	landing := readPid(browserPaths.landingPid)
	b.refresh = true
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the stubborn server to be gone", func() bool { return !alive(landing) || isZombie(landing) })
	if l := readPid(browserPaths.landingPid); l == landing || !alive(l) {
		t.Fatalf("the landing page should run again: %d -> %d", landing, l)
	}
}

// isZombie reports whether pid has exited and waits for its parent.
func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] == "Z"
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

// An instance that cannot run the daemon's fleet binary keeps privoxy; the
// look at the instance that decides it is the only other exec.
func TestBrowserProxyFallsBackToPrivoxyWhereFleetCannotRun(t *testing.T) {
	dir := browserRig(t)
	orig := builtinBrowserProxy
	var seen instancePlatform
	builtinBrowserProxy = func(_ backend.Backend, p instancePlatform) bool { seen = p; return false }
	defer func() { builtinBrowserProxy = orig }()
	b := &localBrowserBackend{}
	if err := ensureBrowserServices(b, "/ws", false); err != nil {
		t.Fatal(err)
	}
	if len(b.scripts) != 2 || !strings.Contains(b.scripts[0], "PLATFORM") || !strings.Contains(b.scripts[1], "privoxy --pidfile") {
		t.Fatalf("the look, then the privoxy setup, should run: %q", b.scripts)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.log")); !os.IsNotExist(err) {
		t.Fatal("the built-in proxy must not start there")
	}
	if !seen.runs(runtime.GOOS, runtime.GOARCH) {
		t.Fatalf("the look read this host as %+v", seen)
	}
}

// The built-in proxy runs where the daemon's binary does: a backend that
// stages its own build, or a Linux daemon's instance that is Linux on its
// architecture. Any other (a Coder or Codespaces workspace of another
// architecture, an instance without uname) gets privoxy, as before.
func TestBuiltinBrowserProxyRule(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	machine := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}
	if !builtinBrowserProxy(&localBrowserBackend{}, instancePlatform{}) {
		t.Error("a backend that stages its own fleet build runs the built-in proxy")
	}
	type plain struct{ backend.Backend }
	same := instancePlatform{os: "Linux", machine: machine[runtime.GOARCH]}
	if got := builtinBrowserProxy(plain{}, same); got != (runtime.GOOS == "linux" && machine[runtime.GOARCH] != "") {
		t.Errorf("an instance of the daemon's platform: %v on %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
	for _, p := range []instancePlatform{{os: "Linux", machine: machine[other]}, {os: "Darwin", machine: machine[runtime.GOARCH]}, {}} {
		if builtinBrowserProxy(plain{}, p) {
			t.Errorf("an instance of %+v runs the %s/%s daemon's binary", p, runtime.GOOS, runtime.GOARCH)
		}
	}
	for m, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64", "armv7l": "arm", "i686": "386", "riscv64": "riscv64"} {
		if got := unameArch(m); got != want {
			t.Errorf("unameArch(%q) = %q, want %q", m, got, want)
		}
	}
	if browserBindAddr(plain{}) != "" || browserBindAddr(loopbackBrowserBackend{}) != "127.0.0.1" {
		t.Error("only a PortDialer's instance binds loopback")
	}
}

// privoxy's setup counts it as running only when its pid file names a live
// privoxy: a pid a restarted container gave to another process (here this
// test's own shell) is started over.
func TestPrivoxySetupIgnoresAStalePid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the check reads /proc")
	}
	start := strings.Index(proxySetupScript, `p="$(cat /tmp/fleet-proxy.pid`)
	end := strings.Index(proxySetupScript, `printf '`)
	if start < 0 || end < start {
		t.Fatalf("the running check is not where it was: %q", proxySetupScript)
	}
	check := strings.ReplaceAll(proxySetupScript[start:end], "/tmp/fleet-proxy.pid", "$PIDFILE") + `echo NOT_RUNNING`
	pidfile := filepath.Join(t.TempDir(), "fleet-proxy.pid")
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() }()
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		cmd := exec.Command("sh", "-c", check)
		cmd.Env = append(os.Environ(), "PIDFILE="+pidfile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the check: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "NOT_RUNNING" {
		t.Fatalf("a live pid that is no privoxy: %q", got)
	}

	// A live privoxy (a sleep under its name) is running.
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	fake := filepath.Join(t.TempDir(), "privoxy")
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, data, 0o755); err != nil {
		t.Fatal(err)
	}
	privoxy := exec.Command(fake, "30")
	if err := privoxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = privoxy.Process.Kill(); _, _ = privoxy.Process.Wait() }()
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(privoxy.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "ALREADY_RUNNING" {
		t.Fatalf("a live privoxy: %q", got)
	}
}
