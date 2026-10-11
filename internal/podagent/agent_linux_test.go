//go:build linux

package podagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

func TestServeHandshake(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	h := s.c.Hello()
	if h.Role != podwire.RoleMain || h.ProtoMin != podwire.ProtoMin || h.ProtoMax != podwire.ProtoMax ||
		h.PodUID != "pod-uid-1" || h.PID != os.Getpid() || h.UID != os.Geteuid() || h.Arch != runtime.GOARCH ||
		h.CanSetuid != execas.CanSwitchUser() || h.Version != "test" || len(h.SHA) != 64 {
		t.Fatalf("hello %+v", h)
	}
	var v VersionInfo
	if err := s.c.Call(context.Background(), "version", nil, &v); err != nil {
		t.Fatal(err)
	}
	if v.Role != podwire.RoleMain || v.Proto != podwire.ProtoMax || v.PID != os.Getpid() || v.SHA != h.SHA {
		t.Fatalf("version %+v", v)
	}
}

func TestServeOffersAtMostTheDaemonsProto(t *testing.T) {
	o := Options{DaemonProto: 1}
	if lo, hi := o.protoRange(); lo != podwire.ProtoMin || hi != 1 {
		t.Fatalf("range %d-%d", lo, hi)
	}
	if lo, hi := (Options{}).protoRange(); lo != podwire.ProtoMin || hi != podwire.ProtoMax {
		t.Fatalf("default range %d-%d", lo, hi)
	}
}

func TestServeRefusedByFleetd(t *testing.T) {
	d := newTestDirs(t)
	s, err := tryStartAgent(t, d.options(), podwire.ClientOptions{ProtoMin: podwire.ProtoMax + 1, ProtoMax: podwire.ProtoMax + 1})
	var refused *podwire.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("dial: %v", err)
	}
	select {
	case err := <-s.served:
		var agentRefused *RefusedError
		if !errors.As(err, &agentRefused) || !strings.Contains(agentRefused.Reason, "no common protocol version") {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused agent kept serving")
	}
}

func TestExecRunPipes(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	res, err := s.c.Run(context.Background(), sh(`cat; echo oops >&2; exit 3`), strings.NewReader("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "hello\n" || string(res.Stderr) != "oops\n" || res.Code != 3 {
		t.Fatalf("%q %q %d", res.Stdout, res.Stderr, res.Code)
	}
	res, err = s.c.Run(context.Background(), sh(`kill -KILL $$`), nil)
	if err != nil || res.Code != -1 || res.Signal != "KILL" {
		t.Fatalf("killed: %+v %v", res, err)
	}
}

// TestExecStdinEOFEndsTheCommand streams more input than the agent's window
// and half-closes it: the command reads every byte, then EOF, and exits (as
// `fleet mic sink` does on its stdin's end).
func TestExecStdinEOFEndsTheCommand(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	e, err := s.c.Exec(context.Background(), sh(`wc -c`))
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("pcm!"), 1<<20) // 4 MiB
	go func() {
		_, _ = e.Stdin().Write(data)
		_ = e.Stdin().Close()
	}()
	out, _ := io.ReadAll(e.Stdout())
	code, err := e.Wait()
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != strconv.Itoa(len(data)) {
		t.Fatalf("out %q code %d err %v", out, code, err)
	}
	if _, err := e.Stdin().Write([]byte("x")); err == nil {
		t.Fatal("stdin took a write after the command ended")
	}
	if err := e.Stdin().Close(); err != nil {
		t.Fatalf("closing stdin after the end: %v", err)
	}
}

// TestExecSlowReaderGetsAllItsInput feeds a command that reads nothing for a
// while more input than the agent's window: the client waits for the window
// instead of overrunning it.
func TestExecSlowReaderGetsAllItsInput(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	for _, h := range []podwire.ExecHeader{sh(`sleep 0.5; wc -c`), resumable(sh(`sleep 0.5; wc -c`))} {
		data := bytes.Repeat([]byte("slow"), 1<<20)
		res, err := s.c.Run(context.Background(), h, bytes.NewReader(data))
		if err != nil || res.Code != 0 || strings.TrimSpace(string(res.Stdout)) != strconv.Itoa(len(data)) {
			t.Fatalf("resumable=%v: %+v %v", h.Resumable, res, err)
		}
	}
}

func TestExecTTYAndResize(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	h := sh(`stty size; read x; stty size; echo "tty:$(tty)"`)
	h.TTY, h.Rows, h.Cols = true, 24, 80
	e, err := s.c.Exec(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, e.Stdout(), "24 80", 5*time.Second)
	if err := e.Resize(50, 132); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := e.Stdin().Write([]byte("go\r")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, e.Stdout(), "tty:/dev/pts/", 5*time.Second)
	if !strings.Contains(out, "50 132") {
		t.Fatalf("after resize: %q", out)
	}
	if code, err := e.Wait(); err != nil || code != 0 {
		t.Fatalf("code %d %v", code, err)
	}
	if b, _ := io.ReadAll(e.Stderr()); len(b) != 0 {
		t.Fatalf("a TTY command has no stderr: %q", b)
	}
}

func TestExecStartErrors(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	for name, tc := range map[string]struct {
		req  execas.Request
		code int
	}{
		"missing command": {execas.Request{Class: execas.ClassScript, Argv: []string{"no-such-command-here"}}, 127},
		"missing cwd":     {execas.Request{Class: execas.ClassScript, Argv: []string{"true"}, Cwd: "/no/such/dir"}, 126},
		"bad request":     {execas.Request{Class: "nope", Argv: []string{"true"}}, 125},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.c.Exec(context.Background(), podwire.ExecHeader{Request: tc.req})
			var se *podwire.StartError
			if !errors.As(err, &se) || se.Code != tc.code {
				t.Fatalf("got %v, want exit code %d", err, tc.code)
			}
		})
	}
}

func TestExecSignal(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	e, err := s.c.Exec(context.Background(), sh(`trap 'echo got INT; exit 7' INT; echo ready; while :; do sleep 0.05; done`))
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, e.Stdout(), "ready", 5*time.Second)
	if err := e.Signal("INT"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, e.Stdout(), "got INT", 5*time.Second)
	if code, _ := e.Wait(); code != 7 {
		t.Fatalf("code %d", code)
	}
}

// startBackground starts sh running a background sleep and returns the exec
// and both pids.
func startBackground(t *testing.T, c *podwire.Client, h podwire.ExecHeader) (*podwire.Exec, int, int) {
	t.Helper()
	e, err := c.Exec(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	line := readUntil(t, e.Stdout(), "\n", 5*time.Second)
	bg, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("background pid line %q", line)
	}
	if !alive(e.PID()) || !alive(bg) {
		t.Fatalf("pids %d %d not running", e.PID(), bg)
	}
	return e, e.PID(), bg
}

const backgroundScript = `sleep 300 & echo $!; wait`

func TestExecCloseAbortsTheProcessGroup(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	e, pid, bg := startBackground(t, s.c, sh(backgroundScript))
	start := time.Now()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Stdout().Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("stdout after close: %d %v", n, err)
	}
	code, err := e.Wait()
	if err != nil || code != -1 {
		t.Fatalf("code %d %v", code, err)
	}
	if st, _ := e.Status(); st.Signal != "TERM" {
		t.Fatalf("status %+v", st)
	}
	waitGone(t, pid, 2*time.Second)
	waitGone(t, bg, 2*time.Second)
	if time.Since(start) > execas.AbortGrace {
		t.Fatalf("abort took %v", time.Since(start))
	}
}

// TestExecFINWithoutEXITAborts closes a raw exec stream (its FIN) before the
// agent sent EXIT: the agent aborts the command and still sends the status.
func TestExecFINWithoutEXITAborts(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	conn, err := s.c.Open(context.Background(), podwire.TagExec, sh(backgroundScript))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := podwire.NewFrameReader(conn)
	f, err := r.Next()
	var open podwire.OpenReply
	if err != nil || f.Type != podwire.FrameOpen || podwire.DecodeFrame(f, &open) != nil || open.PID == 0 {
		t.Fatalf("open: %v %v %+v", f.Type, err, open)
	}
	for f, err = r.Next(); err == nil && f.Type == podwire.FramePing; f, err = r.Next() {
	}
	bg, perr := strconv.Atoi(strings.TrimSpace(string(f.Payload)))
	if err != nil || perr != nil {
		t.Fatalf("background pid: %v %q", err, f.Payload)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil { // FIN, no ABORT frame
		t.Fatal(err)
	}
	for {
		f, err := r.Next()
		if err != nil {
			t.Fatalf("no EXIT after FIN: %v", err)
		}
		if f.Type == podwire.FrameExit {
			var st podwire.ExitStatus
			_ = podwire.DecodeFrame(f, &st)
			if st.Signal != "TERM" {
				t.Fatalf("status %+v", st)
			}
			break
		}
	}
	waitGone(t, open.PID, 2*time.Second)
	waitGone(t, bg, 2*time.Second)
}

func TestTransportLossAbortsANonResumableCommand(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	e, pid, bg := startBackground(t, s.c, sh(backgroundScript))
	s.cut()
	code, err := e.Wait()
	if !errors.Is(err, podwire.ErrConnectionLost) || code != -1 {
		t.Fatalf("code %d err %v", code, err)
	}
	waitGone(t, pid, 3*time.Second)
	waitGone(t, bg, 3*time.Second)
	select {
	case <-s.served:
	case <-time.After(execas.AbortGrace + 5*time.Second):
		t.Fatal("the agent did not end with its session")
	}
}

func TestRunCapsBufferedOutput(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	h := sh(`head -c 17000000 /dev/zero; sleep 300`)
	_, err := s.c.Run(context.Background(), h, nil)
	if !errors.Is(err, podwire.ErrReplyTooLarge) {
		t.Fatalf("got %v", err)
	}
	// A stream has no cap: the same output streams whole.
	e, err := s.c.Exec(context.Background(), sh(`head -c 17000000 /dev/zero`))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, e.Stdout())
	if code, err := e.Wait(); n != 17000000 || code != 0 || err != nil {
		t.Fatalf("streamed %d bytes, code %d, %v", n, code, err)
	}
}

func TestRunHonoursContext(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.c.Run(ctx, sh(`sleep 300`), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestRPC(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.Methods = map[string]Method{
		"add": func(_ context.Context, args json.RawMessage) (any, error) {
			var in [2]int
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return in[0] + in[1], nil
		},
		"fail": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("it failed") },
		"huge": func(context.Context, json.RawMessage) (any, error) { return strings.Repeat("x", podwire.MaxReply), nil },
	}
	s := startAgent(t, o)
	ctx := context.Background()
	var sum int
	if err := s.c.Call(ctx, "add", [2]int{2, 3}, &sum); err != nil || sum != 5 {
		t.Fatalf("add: %d %v", sum, err)
	}
	if err := s.c.Call(ctx, "fail", nil, nil); !podwire.IsCode(err, podwire.CodeFailed) || !strings.Contains(err.Error(), "it failed") {
		t.Fatalf("fail: %v", err)
	}
	if err := s.c.Call(ctx, "huge", nil, nil); !podwire.IsCode(err, podwire.CodeFailed) {
		t.Fatalf("huge: %v", err)
	}
	if err := s.c.Call(ctx, "nope", nil, nil); !podwire.IsCode(err, podwire.CodeUnknownMethod) {
		t.Fatalf("unknown: %v", err)
	}
	var held HeldInfo
	if err := s.c.Call(ctx, "held", nil, &held); err != nil || held.Count != 0 || held.Max != podwire.MaxHeld {
		t.Fatalf("held: %+v %v", held, err)
	}
}

// TestAgentLogsReachFleetd: the agent's own warning (here, a stream kind it
// does not serve: host dials are the agent's own to open) arrives as a LOG
// record at its level.
func TestAgentLogsReachFleetd(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	conn, err := s.c.Open(context.Background(), podwire.TagHostDial, map[string]int{"port": 80})
	if err != nil {
		t.Fatal(err)
	}
	f, err := podwire.NewFrameReader(conn).Next()
	var ref podwire.Refusal
	if err != nil || podwire.DecodeFrame(f, &ref) != nil || ref.Refused != podwire.RefusedUnsupported {
		t.Fatalf("refusal %+v %v", ref, err)
	}
	_ = conn.Close()
	rec, ok := s.logs.find("refused a stream from fleetd", 5*time.Second)
	if !ok {
		t.Fatal("no LOG record")
	}
	attrs := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if rec.Level != slog.LevelWarn || rec.Message != "pod-agent: refused a stream from fleetd" || attrs["kind"] != "host-dial" {
		t.Fatalf("record %v %q %v", rec.Level, rec.Message, attrs)
	}
}

func TestAgentClosesASilentSession(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.PingInterval, o.Silence = 50*time.Millisecond, 300*time.Millisecond
	s, err := tryStartAgent(t, o, podwire.ClientOptions{PingInterval: time.Hour, Silence: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept a silent session")
	}
	select {
	case <-s.c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the client did not see the session end")
	}
}

func TestBulkRole(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.Role = podwire.RoleBulk
	if _, err := tryStartAgent(t, o, podwire.ClientOptions{Role: podwire.RoleMain}); err == nil {
		t.Fatal("a main-role client accepted a bulk agent")
	}
	s := startAgent(t, o)
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef"), 3<<16) // 3 MiB
	path := d.run + "/big.bin"
	if err := s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Mode: 0o600, Size: int64(len(big))}, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	got, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: path})
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	// Exec rides the bulk session too (the folder tars).
	res, err := s.c.Run(ctx, sh(`wc -c < `+path), nil)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != strconv.Itoa(len(big)) {
		t.Fatalf("%+v %v", res, err)
	}
	// Each stream counts until the agent's FIN, which follows its reply.
	deadline := time.Now().Add(3 * time.Second)
	for s.c.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.c.Active() != 0 || s.c.IdleSince().IsZero() {
		t.Fatalf("active %d idle since %v", s.c.Active(), s.c.IdleSince())
	}
}

func TestCleanCloseAbortsWhatIsNotHeld(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	plain, pid, _ := startBackground(t, s.c, sh(backgroundScript))
	h := sh(backgroundScript)
	h.Resumable = true
	held, heldPid, _ := startBackground(t, s.c, h)
	_ = s.c.Close()
	waitGone(t, pid, execas.AbortGrace+2*time.Second)
	if _, err := plain.Wait(); err == nil {
		t.Fatal("the plain command's stream survived the close")
	}
	select {
	case <-held.Detached():
	case <-time.After(5 * time.Second):
		t.Fatal("the resumable command was not detached")
	}
	time.Sleep(200 * time.Millisecond)
	if !alive(heldPid) {
		t.Fatal("a clean close ended the held command")
	}
	held.Fail(nil)
}
