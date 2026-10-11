//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// The tests below run the pool's desired state and the agents' relays
// against the harness's local pods: the real agent binds the in-pod sockets
// in <root>/c.

// controlPath is a file of the pod's control directory.
func (h *harness) controlPath(target PodTarget, name string) string {
	return h.rootPath(target.Pod(), filepath.Join("c", name))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func fileIs(path, want string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == want
}

// sockets is a DesiredSource whose answer a test sets.
type sockets struct {
	mu   sync.Mutex
	want podwire.Sockets
	asks []PodTarget
}

func (s *sockets) source(t PodTarget) podwire.Sockets {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks = append(s.asks, t)
	return s.want
}

func (s *sockets) set(w podwire.Sockets) {
	s.mu.Lock()
	s.want = w
	s.mu.Unlock()
}

// TestEnsureAgentAppliesTheDesiredStateFirst: a session is handed out only
// once its agent has the state applied: the sockets the daemon's source
// names are bound and the control files written, so what runs first in the
// pod (the creation hooks) finds them; the session says what it bound.
func TestEnsureAgentAppliesTheDesiredStateFirst(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	src := &sockets{want: podwire.Sockets{Control: true, SSHAgent: true}}
	h.pool.SetDesiredSource(src.source)
	if err := h.pool.SetControlFile(target.Cluster, target.StatefulSet, "claude-status.json", []byte(`{"live":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{podwire.SockControlName, podwire.SockAgentName} {
		if !isSocket(h.controlPath(target, name)) {
			t.Fatalf("%s was not bound before the session was handed out", name)
		}
	}
	if isSocket(h.controlPath(target, podwire.SockMCPName)) {
		t.Fatal("mcp.sock bound with Fleet MCP off")
	}
	if !fileIs(h.controlPath(target, "claude-status.json"), `{"live":false}`) {
		t.Fatal("the control file was not written before the session was handed out")
	}
	if got := s.Sockets(); !got.Control || !got.SSHAgent || got.MCP {
		t.Fatalf("session sockets %+v", got)
	}
	src.mu.Lock()
	asked := src.asks[0]
	src.mu.Unlock()
	if asked.Fleet != "fleet" || asked.Instance != "one" || asked.StatefulSet != target.StatefulSet {
		t.Fatalf("the source was asked for %+v", asked)
	}
	if got := hookRemoteEnv(s.Sockets()); len(got) != 1 || got[0] != "SSH_AUTH_SOCK=/fleet-mounts/control/ssh-agent.sock" {
		t.Fatalf("hook remote env %q", got)
	}
	if got := hookRemoteEnv(podwire.Sockets{Control: true}); got != nil {
		t.Fatalf("hook remote env without the agent socket %q", got)
	}
}

// TestDesiredStateFollowsChanges: a live session gets every control file
// write (an unchanged one too, which repairs a pod that lost it) and every
// change of the source's answer at once; a session that reconnects gets the
// whole state again.
func TestDesiredStateFollowsChanges(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	src := &sockets{}
	h.pool.SetDesiredSource(src.source)
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	status := h.controlPath(target, "claude-status.json")
	write := func(body string) {
		t.Helper()
		if err := h.pool.SetControlFile(target.Cluster, target.StatefulSet, "claude-status.json", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"n":1}`)
	waitFor(t, "the written file", func() bool { return fileIs(status, `{"n":1}`) })
	_ = os.Remove(status)
	write(`{"n":1}`)
	waitFor(t, "the repaired file", func() bool { return fileIs(status, `{"n":1}`) })

	src.set(podwire.Sockets{MCP: true})
	h.pool.RefreshDesired()
	waitFor(t, "mcp.sock", func() bool { return isSocket(h.controlPath(target, podwire.SockMCPName)) })
	src.set(podwire.Sockets{})
	h.pool.RefreshDesired()
	waitFor(t, "mcp.sock to go", func() bool { _, err := os.Lstat(h.controlPath(target, podwire.SockMCPName)); return os.IsNotExist(err) })

	h.pool.RemoveControlFile(target.Cluster, target.StatefulSet, "claude-status.json")
	waitFor(t, "the removed file", func() bool { _, err := os.Lstat(status); return os.IsNotExist(err) })

	// The session goes (and with it the sockets); the next one gets the
	// whole state again.
	write(`{"n":2}`)
	src.set(podwire.Sockets{Control: true})
	_ = s.Close()
	waitDone(t, s.Done(), "the closed session")
	_ = os.Remove(status)
	if _, err := h.pool.EnsureAgent(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if !fileIs(status, `{"n":2}`) || !isSocket(h.controlPath(target, podwire.SockControlName)) {
		t.Fatal("a new session did not get the whole state")
	}

	// A destroyed instance's files are forgotten.
	h.pool.Forget(target)
	if got := h.pool.ControlFiles(target.Cluster, target.StatefulSet); len(got) != 0 {
		t.Fatalf("a forgotten instance kept %q", got)
	}
}

// TestInboundRelaysCarryTheVerifiedTarget: a connection to an in-pod socket
// reaches the daemon's handler tagged with the pod the session was opened
// for; without a handler it is refused.
func TestInboundRelaysCarryTheVerifiedTarget(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	h.pool.SetDesiredSource(func(PodTarget) podwire.Sockets { return podwire.Sockets{Control: true} })
	if _, err := h.pool.EnsureAgent(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	sock := h.controlPath(target, podwire.SockControlName)
	dial := func() string {
		t.Helper()
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		out, _ := io.ReadAll(conn)
		return string(out)
	}
	if out := dial(); out != "" {
		t.Fatalf("served %q with no handler", out)
	}

	got := make(chan PodTarget, 1)
	h.pool.SetInbound(func(t PodTarget, st *podwire.Stream) {
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		defer conn.Close()
		got <- t
		_, _ = conn.Write([]byte("served " + t.Fleet + "/" + t.Instance))
	})
	if out := dial(); out != "served fleet/one" {
		t.Fatalf("served %q", out)
	}
	if tg := <-got; tg.Cluster != testCluster || tg.StatefulSet != target.StatefulSet {
		t.Fatalf("the handler got %+v", tg)
	}
}

// TestMCPStreamAtTheDropReconnectsFirst: a session that drops while an
// in-pod MCP stream is live reconnects at interactive priority; one that
// drops idle does not.
func TestMCPStreamAtTheDropReconnectsFirst(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	h.pool.SetDesiredSource(func(PodTarget) podwire.Sockets { return podwire.Sockets{MCP: true} })
	opened := make(chan struct{}, 1)
	h.pool.SetInbound(func(_ PodTarget, st *podwire.Stream) {
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		defer conn.Close()
		opened <- struct{}{}
		_, _ = io.Copy(io.Discard, conn)
	})

	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	waitDone(t, s.Done(), "the idle session")
	time.Sleep(mcpLiveWindow + 100*time.Millisecond)
	if h.pool.droppedInteractive(target) {
		t.Fatal("an idle session's drop counts as interactive")
	}

	s, err = h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", h.controlPath(target, podwire.SockMCPName))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-opened
	_ = s.Close()
	waitDone(t, s.Done(), "the session with an MCP stream")
	waitFor(t, "the drop to be noted", func() bool { return h.pool.droppedInteractive(target) })
}
