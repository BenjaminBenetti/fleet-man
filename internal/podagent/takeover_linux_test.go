//go:build linux

package podagent

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// helperAgent is a main agent running as a process of its own, as in a pod.
type helperAgent struct {
	cmd    *exec.Cmd
	c      *podwire.Client
	exited chan *os.ProcessState
}

// startHelperAgent starts `pod-agent serve` (the test binary in the agent
// role) over a socketpair and dials it.
func startHelperAgent(t *testing.T, d testDirs, env ...string) *helperAgent {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	child := os.NewFile(uintptr(fds[0]), "agent-end")
	parent := os.NewFile(uintptr(fds[1]), "fleetd-end")
	cmd := exec.Command(os.Args[0], "pod-agent", "serve")
	cmd.Env = d.helperEnviron("agent", env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = child, child, testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	conn, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		t.Fatal(err)
	}
	h := &helperAgent{cmd: cmd, exited: make(chan *os.ProcessState, 1)}
	go func() {
		_ = cmd.Wait()
		h.exited <- cmd.ProcessState
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_ = conn.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := podwire.Dial(ctx, conn, podwire.ClientOptions{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h.c = c
	// Serving only starts once it holds the lock.
	if err := c.Call(ctx, "version", nil, nil); err != nil {
		t.Fatal(err)
	}
	return h
}

func readPidFile(t *testing.T, d testDirs) pidRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(d.control, pidFileName))
	if err != nil {
		t.Fatal(err)
	}
	var rec pidRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("pid file %q: %v", data, err)
	}
	return rec
}

// takeoverOptions are an in-process agent that takes over.
func takeoverOptions(d testDirs, ask, kill time.Duration) Options {
	o := d.options()
	o.NoTakeover = false
	o.TakeoverAsk, o.TakeoverKill = ask, kill
	return o
}

// TestTakeoverReleases: a new agent asks the recorded one (by its nonce) to
// release; it does, and exits; the held command stays with its shepherd and
// re-attaches through the new agent.
func TestTakeoverReleases(t *testing.T) {
	d := newTestDirs(t)
	old := startHelperAgent(t, d)
	rec := readPidFile(t, d)
	if rec.PID != old.cmd.Process.Pid || !podwire.ValidResumeID(rec.Nonce) {
		t.Fatalf("pid file %+v, agent pid %d", rec, old.cmd.Process.Pid)
	}
	e, err := old.c.Exec(context.Background(), resumable(sh(`read x; echo "after:$x"`)))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	s := startAgent(t, takeoverOptions(d, 2*time.Second, 2*time.Second))
	if err := s.c.Call(context.Background(), "version", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-old.exited:
		if !st.Success() {
			t.Fatalf("the released agent exited %v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old agent did not exit")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("a willing release took %v", time.Since(start))
	}
	if got := readPidFile(t, d); got.PID != os.Getpid() || got.Nonce == rec.Nonce {
		t.Fatalf("pid file after takeover %+v", got)
	}
	waitDetached(t, e)
	if err := s.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	_, _ = e.Stdin().Write([]byte("takeover\n"))
	readUntil(t, e.Stdout(), "after:takeover", 5*time.Second)
	if code, err := e.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
}

// TestTakeoverHungHolder: the recorded agent is stopped (it holds the lock but
// answers nothing): the new agent gives it the ask grace, SIGTERMs it, then
// SIGKILLs it, and takes the lock. The held command survives.
func TestTakeoverHungHolder(t *testing.T) {
	d := newTestDirs(t)
	old := startHelperAgent(t, d)
	e, err := old.c.Exec(context.Background(), resumable(sh(`read x; echo "after:$x"`)))
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	if err := old.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s := startAgent(t, takeoverOptions(d, 300*time.Millisecond, 500*time.Millisecond))
	if err := s.c.Call(context.Background(), "version", nil, nil); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	select {
	case st := <-old.exited:
		ws, _ := st.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the hung agent ended %v, want SIGKILL", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hung agent was not killed")
	}
	if took < 800*time.Millisecond || took > 4*time.Second {
		t.Fatalf("takeover took %v (ask 300ms + SIGTERM grace 500ms, then SIGKILL)", took)
	}
	if got := readPidFile(t, d); got.PID != os.Getpid() {
		t.Fatalf("pid file %+v", got)
	}
	if !alive(pid) {
		t.Fatal("the held command died with its agent")
	}
	waitDetached(t, e)
	if err := s.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	_, _ = e.Stdin().Write([]byte("hung\n"))
	readUntil(t, e.Stdout(), "after:hung", 5*time.Second)
	if code, err := e.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
}

// TestTakeoverNeverSignalsAStaleRecord: a pid file naming a live process that
// holds no lock (its pid reused) is just taken; that process is never
// signalled.
func TestTakeoverNeverSignalsAStaleRecord(t *testing.T) {
	d := newTestDirs(t)
	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bystander.Process.Kill(); _ = bystander.Wait() }()
	if err := os.MkdirAll(d.control, 0o755); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(pidRecord{PID: bystander.Process.Pid, Nonce: newResumeID()})
	if err := os.WriteFile(filepath.Join(d.control, pidFileName), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s := startAgent(t, takeoverOptions(d, 2*time.Second, 2*time.Second))
	if err := s.c.Call(context.Background(), "version", nil, nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("taking an unlocked pid file took %v", time.Since(start))
	}
	if !alive(bystander.Process.Pid) {
		t.Fatal("the bystander was signalled")
	}
	if got := readPidFile(t, d); got.PID != os.Getpid() {
		t.Fatalf("pid file %+v", got)
	}
}
