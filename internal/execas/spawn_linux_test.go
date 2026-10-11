//go:build linux

package execas

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// selfHost is a pod this test's own process can be: single-uid (the tests run
// unprivileged, as a tier C pod does), its /etc listing only this user.
func selfHost(t *testing.T) (*Host, string) {
	t.Helper()
	work := t.TempDir()
	return &Host{
		Root:       selfRoot(t, t.TempDir(), "", "tty:x:5:\n"),
		Spec:       Spec{WorkingDir: work, WorkspaceFolder: work, UserEnvProbe: ProbeNone},
		ProcessEnv: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"},
		Self:       CurrentCreds(),
		SingleUID:  true,
	}, work
}

func mustPlan(t *testing.T, h *Host, req Request) *Plan {
	t.Helper()
	plan, err := h.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestStartRunsTheCommandAsPlanned(t *testing.T) {
	h, work := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{helperBinary(t), "an arg"},
		Env: map[string]string{helperEnv: "report"}})
	var out bytes.Buffer
	proc, err := Start(plan, Stdio{Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	st, err := proc.Wait()
	if err != nil || st != (ExitStatus{}) {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
	r := parseReport(t, out.Bytes())
	me := CurrentCreds()
	if r.UID != int(me.UID) || r.GID != int(me.GID) {
		t.Errorf("ran as %d:%d, want this process's %d:%d", r.UID, r.GID, me.UID, me.GID)
	}
	if r.Cwd != work {
		t.Errorf("cwd = %q, want the container working dir %q", r.Cwd, work)
	}
	if !sameEnv(r.Env, plan.Env) {
		t.Errorf("the command's env is not the plan's:\n got %v\nwant %v", r.Env, plan.Env)
	}
	if !reflect.DeepEqual(r.Args, plan.Argv) {
		t.Errorf("args = %v, want %v", r.Args, plan.Argv)
	}
	if r.Pid != proc.Pid() || r.Sid != r.Pid {
		t.Errorf("pid %d sid %d, want a session of its own led by pid %d", r.Pid, r.Sid, proc.Pid())
	}
}

func TestStartWiresStdioAndTheExitCode(t *testing.T) {
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"/bin/sh", "-c", "cat; echo oops >&2; exit 7"}})
	var stdout, stderr bytes.Buffer
	proc, err := Start(plan, Stdio{Stdin: strings.NewReader("hello\x00world"), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	st, err := proc.Wait()
	if err != nil || st.Code != 7 || st.ShellCode() != 7 {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
	if stdout.String() != "hello\x00world" || stderr.String() != "oops\n" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestStartOnATTY(t *testing.T) {
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassExec, Argv: []string{helperBinary(t)}, TTY: true, Rows: 40, Cols: 100,
		Env: map[string]string{helperEnv: "report"}})
	proc, err := Start(plan, Stdio{})
	if err != nil {
		t.Fatal(err)
	}
	master := proc.PTY()
	if master == nil {
		t.Fatal("no PTY")
	}
	out, _ := io.ReadAll(master) // ends with EIO once the command is gone
	if st, err := proc.Wait(); err != nil || st.Code != 0 {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
	master.Close()
	r := parseReport(t, out)
	if r.TTY == nil {
		t.Fatal("the command's stdin is not a terminal")
	}
	if r.TTY.UID != CurrentCreds().UID {
		t.Errorf("single-uid PTY owned by %d, want this process's %d", r.TTY.UID, CurrentCreds().UID)
	}
	if r.TTY.Rows != 40 || r.TTY.Cols != 100 {
		t.Errorf("size %dx%d, want 40x100", r.TTY.Rows, r.TTY.Cols)
	}
	if !r.TTY.Ctty || r.Sid != r.Pid {
		t.Errorf("ctty %v, sid %d pid %d: want the PTY as the controlling terminal of its own session", r.TTY.Ctty, r.Sid, r.Pid)
	}
	if !hasEnv(r.Env, "TERM=xterm") {
		t.Errorf("TERM not defaulted: %v", r.Env)
	}
}

func TestResize(t *testing.T) {
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"sleep", "30"}, TTY: true})
	proc, err := Start(plan, Stdio{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { proc.Kill(); _, _ = proc.Wait() }()
	if err := proc.Resize(33, 77); err != nil {
		t.Fatal(err)
	}
	ws, err := pty.GetsizeFull(proc.PTY())
	if err != nil || ws.Rows != 33 || ws.Cols != 77 {
		t.Fatalf("size = %+v, %v", ws, err)
	}
}

// startTree starts the "tree" helper, whose sleep runs in a process group of
// its own (as an interactive shell's job would), and returns the sleep's pid.
func startTree(t *testing.T) (*Process, int) {
	t.Helper()
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{helperBinary(t)}, Env: map[string]string{helperEnv: "tree"}})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	proc, err := Start(plan, Stdio{Stdout: w})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatalf("tree helper said %q: %v", line, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "GRANDCHILD ")))
	if err != nil {
		t.Fatalf("tree helper said %q", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return proc, pid
}

func TestAbortTerminatesThenKillsTheWholeSession(t *testing.T) {
	old := abortGrace
	abortGrace = 300 * time.Millisecond
	defer func() { abortGrace = old }()

	proc, grandchild := startTree(t)
	proc.Abort()
	proc.Abort() // idempotent
	st, err := proc.Wait()
	if err != nil || st.Signal != syscall.SIGTERM || st.Code != -1 || st.ShellCode() != 128+15 {
		t.Fatalf("Wait = %+v, %v: want the process group's SIGTERM", st, err)
	}
	// The sleep is in another process group, which SIGTERM -pgid misses; the
	// session kill after the grace gets it.
	if !waitGone(grandchild, 5*time.Second) {
		t.Fatal("a process in the command's session outlived Abort")
	}
}

func TestAbortHangsUpATTY(t *testing.T) {
	old := abortGrace
	abortGrace = time.Minute // the hangup alone must end it
	defer func() { abortGrace = old }()

	h, _ := selfHost(t)
	proc, err := Start(mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"sleep", "300"}, TTY: true}), Stdio{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	proc.Abort()
	st, err := proc.Wait()
	if err != nil || st.Signal != syscall.SIGHUP {
		t.Fatalf("Wait = %+v, %v: want a hangup", st, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hangup took too long")
	}
}

func TestKillEndsTheSessionNow(t *testing.T) {
	proc, grandchild := startTree(t)
	proc.Kill()
	if st, err := proc.Wait(); err != nil || st.Signal != syscall.SIGKILL {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
	if !waitGone(grandchild, 2*time.Second) {
		t.Fatal("Kill missed a process in the session")
	}
}

func TestSignalReachesTheForeground(t *testing.T) {
	h, _ := selfHost(t)
	for _, tty := range []bool{false, true} {
		proc, err := Start(mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"sleep", "300"}, TTY: tty}), Stdio{})
		if err != nil {
			t.Fatal(err)
		}
		if err := proc.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if st, err := proc.Wait(); err != nil || st.Signal != syscall.SIGINT {
			t.Fatalf("tty=%v: Wait = %+v, %v", tty, st, err)
		}
		if tty {
			proc.PTY().Close()
		}
		if err := proc.Signal(syscall.SIGINT); err != nil && tty == false {
			t.Fatalf("signalling a reaped command: %v", err)
		}
	}
}

func TestStartFailuresCarryDockersExitCodes(t *testing.T) {
	notExec := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		plan Plan
		code int
		want string
	}{
		{"missing executable", Plan{Path: "/no/such/tool", Argv: []string{"/no/such/tool"}, Dir: "/"}, ExitNotFound, "no such file"},
		{"missing dir", Plan{Path: "/bin/true", Argv: []string{"true"}, Dir: "/no/such/dir"}, ExitCannotRun, "chdir /no/such/dir"},
		{"not executable", Plan{Path: notExec, Argv: []string{notExec}, Dir: "/"}, ExitCannotRun, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Start(&tc.plan, Stdio{})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want exit %d containing %q", err, tc.code, tc.want)
			}
		})
	}
}

func TestCanSwitchUserNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	if CanSwitchUser() {
		t.Fatal("an unprivileged process claims it can switch users")
	}
	if _, err := effectiveCaps(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionOfParsesOddCommandNames(t *testing.T) {
	// A command name may hold spaces and parentheses; the fields after the
	// last ')' are what count.
	dir := t.TempDir()
	odd := filepath.Join(dir, "a) b (c")
	if err := os.Symlink("/bin/sleep", odd); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(odd, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if sid, ok := sessionOf(cmd.Process.Pid); !ok || sid != cmd.Process.Pid {
		t.Fatalf("sessionOf = %d, %v, want %d", sid, ok, cmd.Process.Pid)
	}
}

// runExecAs runs the test binary as `fleet exec-as` with stdin, and returns
// its output and how it ended.
func runExecAs(t *testing.T, spec string, stdin io.Reader) (stdout, stderr string, state *os.ProcessState) {
	t.Helper()
	cmd := exec.Command(helperBinary(t))
	cmd.Env = append(os.Environ(), helperEnv+"=exec-as", "EXECAS_TEST_SPEC="+spec, "EXECAS_TEST_CACHE=")
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	return out.String(), errOut.String(), cmd.ProcessState
}

func frame(t *testing.T, req Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExecAsRunsTheRequestInPlace(t *testing.T) {
	work := t.TempDir()
	spec := writeSpecFile(t, `{"working_dir":"`+work+`","user_env_probe":"none"}`)
	stdin := append(frame(t, Request{Class: ClassScript, Argv: []string{helperBinary(t), "x"},
		Env: map[string]string{helperEnv: "report", "EXECAS_TEST_STDIN": "1", "FROM_REQUEST": "yes"}}),
		[]byte("the rest of stdin\x00is the command's")...)
	stdout, stderr, state := runExecAs(t, spec, bytes.NewReader(stdin))
	if state.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", state.ExitCode(), stderr)
	}
	r := parseReport(t, []byte(stdout))
	if r.Pid != state.Pid() {
		t.Errorf("the command ran as pid %d, not in place of exec-as (pid %d)", r.Pid, state.Pid())
	}
	if r.Stdin != "the rest of stdin\x00is the command's" {
		t.Errorf("stdin after the frame = %q", r.Stdin)
	}
	if r.Cwd != work || r.UID != os.Getuid() {
		t.Errorf("cwd %q uid %d", r.Cwd, r.UID)
	}
	if !hasEnv(r.Env, "FROM_REQUEST=yes") {
		t.Errorf("request env missing: %v", r.Env)
	}
}

func TestExecAsExitStatus(t *testing.T) {
	work := t.TempDir()
	spec := writeSpecFile(t, `{"working_dir":"`+work+`","user_env_probe":"none"}`)
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer tty.Close()
	for _, tc := range []struct {
		name   string
		stdin  io.Reader
		code   int
		signal syscall.Signal
		stderr string
	}{
		{"the command's own code", bytes.NewReader(frame(t, Request{Class: ClassScript, Argv: []string{"/bin/sh", "-c", "exit 7"}})), 7, 0, ""},
		{"killed by a signal", bytes.NewReader(frame(t, Request{Class: ClassScript, Argv: []string{"/bin/sh", "-c", "kill -TERM $$"}})), -1, syscall.SIGTERM, ""},
		{"not found", bytes.NewReader(frame(t, Request{Class: ClassScript, Argv: []string{"no-such-tool"}})), 127, 0, `exec: "no-such-tool": executable file not found in $PATH`},
		{"missing dir", bytes.NewReader(frame(t, Request{Class: ClassScript, Argv: []string{"true"}, Cwd: "/no/such/dir"})), 126, 0, "chdir /no/such/dir"},
		{"not a frame", strings.NewReader("ls -la\n"), 125, 0, "not an exec request frame"},
		{"empty stdin", strings.NewReader(""), 125, 0, "no exec request on stdin"},
		{"a tty request", bytes.NewReader(frame(t, Request{Class: ClassExec, Argv: []string{"bash"}, TTY: true})), 125, 0, "no tty commands"},
		{"stdin is a terminal", tty, 125, 0, "must not be a terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, state := runExecAs(t, spec, tc.stdin)
			ws := state.Sys().(syscall.WaitStatus)
			if tc.signal != 0 {
				if !ws.Signaled() || ws.Signal() != tc.signal {
					t.Fatalf("status %v, want killed by %v", ws, tc.signal)
				}
				return
			}
			if state.ExitCode() != tc.code || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("exit %d stderr %q, want exit %d containing %q", state.ExitCode(), stderr, tc.code, tc.stderr)
			}
			if tc.code >= 125 && !strings.HasPrefix(stderr, "fleet exec-as: ") {
				t.Fatalf("stderr %q should say who failed", stderr)
			}
		})
	}
	_, stderr, state := runExecAs(t, filepath.Join(work, "missing.json"), bytes.NewReader(frame(t, Request{Class: ClassScript, Argv: []string{"true"}})))
	if state.ExitCode() != 125 || !strings.Contains(stderr, "read exec spec") {
		t.Fatalf("missing spec: exit %d stderr %q", state.ExitCode(), stderr)
	}
}

// TestStartInACgroupThatIsNotOneStillRuns: a cgroup the kernel refuses (here
// a plain dir), or one that does not exist, is never a reason not to run the
// command; with a TTY too.
func TestStartInACgroupThatIsNotOneStillRuns(t *testing.T) {
	h, _ := selfHost(t)
	for name, dir := range map[string]string{"plain dir": t.TempDir(), "missing": filepath.Join(t.TempDir(), "nope")} {
		for _, tty := range []bool{false, true} {
			plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"sh", "-c", "echo ran"}, TTY: tty})
			plan.Cgroup = dir
			var out bytes.Buffer
			proc, err := Start(plan, Stdio{Stdout: &out})
			if err != nil {
				t.Fatalf("%s (tty %v): %v", name, tty, err)
			}
			if tty {
				got, _ := io.ReadAll(proc.PTY())
				out.Write(got)
			}
			if st, err := proc.Wait(); err != nil || st.Code != 0 {
				t.Fatalf("%s (tty %v): %+v, %v", name, tty, st, err)
			}
			if !strings.Contains(out.String(), "ran") {
				t.Fatalf("%s (tty %v): output %q", name, tty, out.String())
			}
		}
	}
}

// TestStartInACgroupKeepsTheCommandsOwnErrors: a missing executable is still
// docker's 127 with a cgroup set.
func TestStartInACgroupKeepsTheCommandsOwnErrors(t *testing.T) {
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"sh"}})
	plan.Path = filepath.Join(t.TempDir(), "gone")
	plan.Cgroup = t.TempDir()
	_, err := Start(plan, Stdio{})
	if ExitCode(err) != ExitNotFound {
		t.Fatalf("a missing executable in a cgroup: %v (code %d)", err, ExitCode(err))
	}
}

// TestStartInARealCgroup runs where this process may make a cgroup v2 child
// (root with a delegated, writable cgroupfs): the command is in it from its
// first instruction.
func TestStartInARealCgroup(t *testing.T) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip(err)
	}
	cur := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			cur = rest
		}
	}
	parent := filepath.Join("/sys/fs/cgroup", cur)
	child := filepath.Join(parent, "execas-test-"+strconv.Itoa(os.Getpid()))
	if cur == "" || os.Mkdir(child, 0o755) != nil {
		t.Skip("no writable cgroup v2 here")
	}
	t.Cleanup(func() { _ = os.Remove(child) })
	h, _ := selfHost(t)
	plan := mustPlan(t, h, Request{Class: ClassScript, Argv: []string{"cat", "/proc/self/cgroup"}})
	plan.Cgroup = child
	var out bytes.Buffer
	proc, err := Start(plan, Stdio{Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cur, filepath.Base(child)); !strings.Contains(out.String(), want) {
		t.Fatalf("the command ran in %q, want %s", out.String(), want)
	}
}
