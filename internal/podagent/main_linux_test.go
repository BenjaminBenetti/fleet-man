//go:build linux

package podagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// helperEnv names the role a re-executed test binary plays (runTestHelper):
// the shepherds the agents under test start, and agents and direct execs the
// tests run as processes of their own.
const helperEnv = "PODAGENT_TEST_HELPER"

func TestMain(m *testing.M) {
	if role := os.Getenv(helperEnv); role != "" {
		os.Exit(runTestHelper(role))
	}
	os.Exit(m.Run())
}

// runTestHelper plays a role and returns its exit code:
//
//	shepherd  `pod-agent shepherd`
//	agent     `pod-agent serve` on stdio, configured by helperOptions
//	stdio     `pod-agent exec --stdio`, configured by helperOptions
func runTestHelper(role string) int {
	switch role {
	case "shepherd":
		if err := RunShepherd(); err != nil {
			return 1
		}
		return 0
	case "agent":
		conn, err := TakeStdio()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ctx, stop := signalContext()
		defer stop()
		if err := Serve(ctx, conn, helperOptions()); err != nil {
			fmt.Fprintln(os.Stderr, "helper agent:", err)
			return 1
		}
		return 0
	case "stdio":
		if err := ExecStdio(context.Background(), os.Stdin, os.Stdout, helperOptions(), os.Getenv("PODAGENT_TEST_RESUME")); err != nil {
			fmt.Fprintln(os.Stderr, "helper exec --stdio:", err)
			return 1
		}
		return 0
	}
	return 2
}

// signalContext is cancelled on SIGTERM, as fleet's main context is.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signalNotify(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func signalNotify(c chan os.Signal) { signal.Notify(c, syscall.SIGTERM, syscall.SIGINT) }

// helperOptions are a helper process's Options, from the environment its
// test set.
func helperOptions() Options {
	o := Options{
		Role:         podwire.Role(os.Getenv("PODAGENT_TEST_ROLE")),
		SpecPath:     os.Getenv("PODAGENT_TEST_SPEC"),
		ControlDir:   os.Getenv("PODAGENT_TEST_CONTROL"),
		RunDir:       os.Getenv("PODAGENT_TEST_RUN"),
		PodUID:       os.Getenv("PODAGENT_TEST_POD_UID"),
		Version:      "test",
		Exe:          os.Args[0],
		ShepherdArgs: []string{"pod-agent", "shepherd"},
		ShepherdEnv:  shepherdEnv(),
		NoTakeover:   os.Getenv("PODAGENT_TEST_NO_TAKEOVER") == "1",
		InitOOMScore: os.Getenv("PODAGENT_TEST_OOM_FROM"),
	}
	durations := map[string]*time.Duration{
		"PODAGENT_TEST_ASK":            &o.TakeoverAsk,
		"PODAGENT_TEST_KILL":           &o.TakeoverKill,
		"PODAGENT_TEST_GRACE":          &o.HoldGrace,
		"PODAGENT_TEST_STREAM_PING":    &o.StreamPing,
		"PODAGENT_TEST_STREAM_SILENCE": &o.StreamSilence,
	}
	for env, d := range durations {
		if v, err := time.ParseDuration(os.Getenv(env)); err == nil {
			*d = v
		}
	}
	return o
}

// shepherdEnv is the environment of the shepherds a test starts.
func shepherdEnv() []string {
	env := []string{helperEnv + "=shepherd"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, helperEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// testDirs are one test's control and run directories. Unix socket paths are
// short, so they live directly under /tmp.
type testDirs struct {
	control, run, spec string
}

func newTestDirs(t *testing.T) testDirs {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pa")
	if err != nil {
		t.Fatal(err)
	}
	// Users other than the test's own (the AsRoot tests) reach into it.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killShepherds(root)
		_ = os.RemoveAll(root)
	})
	d := testDirs{control: filepath.Join(root, "c"), run: filepath.Join(root, "r"), spec: filepath.Join(root, "exec.json")}
	spec, _ := json.Marshal(execas.Spec{UserEnvProbe: execas.ProbeNone, WorkingDir: "/"})
	if err := os.WriteFile(d.spec, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// killShepherds ends the shepherds a test left behind, and their commands
// (each a session of its own, led by a child of its shepherd).
func killShepherds(root string) {
	markers, _ := filepath.Glob(filepath.Join(root, "r", heldDirName, "*.sock"))
	if len(markers) == 0 {
		return
	}
	procs, _ := os.ReadDir("/proc")
	shepherds := map[int]bool{}
	parents := map[int]int{}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if stat, err := os.ReadFile("/proc/" + p.Name() + "/stat"); err == nil {
			if i := bytes.LastIndexByte(stat, ')'); i > 0 {
				if f := strings.Fields(string(stat[i+1:])); len(f) > 1 {
					parents[pid], _ = strconv.Atoi(f[1])
				}
			}
		}
		environ, err := os.ReadFile("/proc/" + p.Name() + "/environ")
		if err == nil && bytes.Contains(environ, []byte(helperEnv+"=shepherd")) && bytes.Contains(environ, []byte(root)) {
			shepherds[pid] = true
		}
	}
	for pid, ppid := range parents {
		if shepherds[ppid] {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	for pid := range shepherds {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// options are an in-process agent's Options over d.
func (d testDirs) options() Options {
	return Options{
		Role:         podwire.RoleMain,
		SpecPath:     d.spec,
		ControlDir:   d.control,
		RunDir:       d.run,
		PodUID:       "pod-uid-1",
		Version:      "test",
		Exe:          os.Args[0],
		ShepherdArgs: []string{"pod-agent", "shepherd"},
		ShepherdEnv:  append(shepherdEnv(), "PODAGENT_TEST_ROOT="+filepath.Dir(d.run)),
		NoTakeover:   true,
	}
}

// helperEnviron is the environment of a helper agent or exec over d.
func (d testDirs) helperEnviron(role string, extra ...string) []string {
	env := []string{
		helperEnv + "=" + role,
		"PODAGENT_TEST_SPEC=" + d.spec,
		"PODAGENT_TEST_CONTROL=" + d.control,
		"PODAGENT_TEST_RUN=" + d.run,
		"PODAGENT_TEST_POD_UID=pod-uid-1",
		"PODAGENT_TEST_ROOT=" + filepath.Dir(d.run),
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PODAGENT_TEST") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// session is a client and the in-process agent it talks to.
type session struct {
	c      *podwire.Client
	agent  net.Conn
	served chan error
	ended  chan struct{}
	logs   *logCapture
}

// startAgent runs Serve with o over a net.Pipe and dials it.
func startAgent(t *testing.T, o Options) *session {
	t.Helper()
	s, err := tryStartAgent(t, o, podwire.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tryStartAgent(t *testing.T, o Options, copts podwire.ClientOptions) (*session, error) {
	t.Helper()
	a, b := net.Pipe()
	served := make(chan error, 1)
	ended := make(chan struct{})
	if o.Stderr == nil {
		o.Stderr = testWriter{t}
	}
	go func() {
		served <- Serve(context.Background(), a, o)
		close(ended)
	}()
	logs := &logCapture{}
	copts.Logger = slog.New(logs)
	if copts.Role == "" {
		copts.Role = o.Role
	}
	if copts.Version == "" {
		copts.Version = "test"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := podwire.Dial(ctx, b, copts)
	s := &session{c: c, agent: a, served: served, ended: ended, logs: logs}
	if err != nil {
		return s, err
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = a.Close()
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not end")
		}
	})
	return s, nil
}

// cut drops the session's transport, as a network cut does.
func (s *session) cut() { _ = s.agent.Close() }

// testWriter writes an agent's stderr to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// logCapture is the slog handler a test's client logs the agent's records to.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.recs = append(c.recs, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// find waits for a record whose message contains msg.
func (c *logCapture) find(msg string, within time.Duration) (slog.Record, bool) {
	deadline := time.Now().Add(within)
	for {
		c.mu.Lock()
		for _, r := range c.recs {
			if strings.Contains(r.Message, msg) {
				c.mu.Unlock()
				return r, true
			}
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			return slog.Record{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sh is a script-class request running sh -c script.
func sh(script string) podwire.ExecHeader {
	return podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", script}}}
}

// alive reports whether pid is a live (not zombie) process.
func alive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(data, ')')
	return i > 0 && len(data) > i+2 && data[i+2] != 'Z'
}

// waitGone waits for pid to end.
func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still running after %v", pid, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readUntil reads r until what it read contains want.
func readUntil(t *testing.T, r io.Reader, want string, within time.Duration) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			buf.Write(b[:n])
			if strings.Contains(buf.String(), want) || err != nil {
				got <- buf.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, want) {
			t.Fatalf("read %q, want %q", s, want)
		}
		return s
	case <-time.After(within):
		t.Fatalf("no %q within %v", want, within)
	}
	return ""
}

// heldCount is how many held markers the run dir has.
func heldCount(run string) int {
	m, _ := filepath.Glob(filepath.Join(run, heldDirName, "*"+heldSuffix))
	return len(m)
}
