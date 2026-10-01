package fleetclient

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
)

// The lock holder is this test binary re-run as a stand-in daemon: it takes
// the lifetime lock like `fleet server` does and holds it until it dies.
const (
	envHolderLock       = "FLEET_TEST_HOLDER_LOCK"
	envHolderIgnoreTerm = "FLEET_TEST_HOLDER_IGNORE_TERM"
	envHolderExitAfter  = "FLEET_TEST_HOLDER_EXIT_AFTER"
)

// TestLockHolderProcess is the stand-in daemon's body; a no-op in a normal run.
func TestLockHolderProcess(t *testing.T) {
	path := os.Getenv(envHolderLock)
	if path == "" {
		return
	}
	if os.Getenv(envHolderIgnoreTerm) != "" {
		signal.Ignore(syscall.SIGTERM)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("holding\n")
	lifetime := time.Minute // never outlive a test run that lost track of it
	if d, err := time.ParseDuration(os.Getenv(envHolderExitAfter)); err == nil {
		lifetime = d
	}
	time.Sleep(lifetime)
	os.Exit(0)
}

// daemonHome is a fresh HOME with ~/.fleet and an unheld lifetime lock in it.
func daemonHome(t *testing.T) {
	t.Helper()
	home := probeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetpaths.ServerLockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// startLockHolder runs the stand-in daemon and returns once it holds the lock.
// The returned channel yields how it ended.
func startLockHolder(t *testing.T, env ...string) <-chan *os.ProcessState {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderProcess$")
	cmd.Env = append(os.Environ(), envHolderLock+"="+fleetpaths.ServerLockPath())
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan *os.ProcessState, 1)
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "holding" {
		t.Fatalf("lock holder did not start: %q, %v", line, err)
	}
	go func() {
		_ = cmd.Wait()
		ended <- cmd.ProcessState
	}()
	return ended
}

func shortenKillWait(t *testing.T) {
	t.Helper()
	orig := killWait
	killWait = 300 * time.Millisecond
	t.Cleanup(func() { killWait = orig })
}

func localEP() Endpoint { return localEndpoint{socket: fleetpaths.SocketPath()} }

func TestServerLockHeld(t *testing.T) {
	probeHome(t)
	if serverLockHeld() {
		t.Fatal("held with no lock file at all")
	}
	daemonHome(t)
	if serverLockHeld() {
		t.Fatal("held with nobody holding it")
	}
	startLockHolder(t)
	if !serverLockHeld() {
		t.Fatal("not held while a daemon holds it")
	}
}

// TestClearServerKillsADaemonThatWillNotExit is the wedged daemon: it holds the
// lock, answers nothing, and (mid-shutdown) no longer reacts to SIGTERM.
func TestClearServerKillsADaemonThatWillNotExit(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want syscall.Signal
	}{
		"exits on SIGTERM": {nil, syscall.SIGTERM},
		"ignores SIGTERM":  {[]string{envHolderIgnoreTerm + "=1"}, syscall.SIGKILL},
	} {
		t.Run(name, func(t *testing.T) {
			daemonHome(t)
			shortenKillWait(t)
			ended := startLockHolder(t, tc.env...)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			revived, killed, err := clearServer(ctx, localEP(), 200*time.Millisecond, true)
			if err != nil || revived || killed == "" {
				t.Fatalf("clearServer = revived %v, killed %q, err %v; want a kill", revived, killed, err)
			}
			if serverLockHeld() {
				t.Fatal("the lifetime lock is still held: a new daemon could not start")
			}
			state := <-ended
			if status, ok := state.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != tc.want {
				t.Fatalf("the daemon ended with %v, want %v", state, tc.want)
			}
		})
	}
}

// TestClearServerWaitsOutAnExitingDaemon: one that is merely still shutting
// down is left to finish.
func TestClearServerWaitsOutAnExitingDaemon(t *testing.T) {
	daemonHome(t)
	ended := startLockHolder(t, envHolderExitAfter+"=300ms")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	revived, killed, err := clearServer(ctx, localEP(), 10*time.Second, false)
	if err != nil || revived || killed != "" {
		t.Fatalf("clearServer = revived %v, killed %q, err %v; want it to wait", revived, killed, err)
	}
	if state := <-ended; !state.Success() {
		t.Fatalf("the daemon ended with %v, want a clean exit", state)
	}
}

// TestClearServerKeepsADaemonThatAnswers: silence that ends within the grace
// (a daemon still starting, a busy one) is not a reason to kill.
func TestClearServerKeepsADaemonThatAnswers(t *testing.T) {
	daemonHome(t)
	ended := startLockHolder(t)
	lis, err := net.Listen("unix", fleetpaths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(srv, &armadaOnlyServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	revived, killed, err := clearServer(ctx, localEP(), 10*time.Second, true)
	if err != nil || !revived || killed != "" {
		t.Fatalf("clearServer = revived %v, killed %q, err %v; want revived", revived, killed, err)
	}
	select {
	case state := <-ended:
		t.Fatalf("the answering daemon was ended: %v", state)
	default:
	}
}

// TestLockHoldersFindsTheDaemonOnly: the daemon is found, and this process —
// which a probe makes a momentary holder of the file — never is.
func TestLockHoldersFindsTheDaemonOnly(t *testing.T) {
	daemonHome(t)
	self, err := os.Open(fleetpaths.ServerLockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	if pids, err := lockHolders(fleetpaths.ServerLockPath()); err != nil || len(pids) != 0 {
		t.Fatalf("holders with no daemon = %v, %v; want none", pids, err)
	}
	startLockHolder(t)
	pids, err := lockHolders(fleetpaths.ServerLockPath())
	if err != nil || len(pids) != 1 || slices.Contains(pids, os.Getpid()) {
		t.Fatalf("holders = %v, %v; want exactly the daemon", pids, err)
	}
}
