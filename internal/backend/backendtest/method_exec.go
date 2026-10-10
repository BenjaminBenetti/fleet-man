package backendtest

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"
)

// caseExecExitCodes: ExecCommand and ExecCommandQuiet report the command's own
// exit code, keep a failing command's output, and report a missing command as
// the shell's 127.
func caseExecExitCodes(c *C) {
	i := c.Shared()
	for _, code := range []int{0, 1, 3, 42, 255} {
		for _, quiet := range []bool{false, true} {
			run := c.Exec
			if quiet {
				run = c.ExecQuiet
			}
			r := run(i, ShArgv(fmt.Sprintf("exit %d", code)), nil)
			if r.Code != code || (r.Err == nil) != (code == 0) {
				c.Errorf("exit %d (quiet %v): %s", code, quiet, r)
			}
		}
	}
	if r := c.Exec(i, ShArgv("printf partial; exit 5"), nil); r.Code != 5 || string(r.Stdout) != "partial" {
		c.Errorf("a failing command's output: want exit 5 and stdout %q, got %s", "partial", r)
	}
	if r := c.Exec(i, ShArgv("fleet-conformance-no-such-command"), nil); r.Code != 127 {
		c.Errorf("a missing command: want exit 127, got %s", r)
	}
}

// caseExecStreams: stdout and stderr arrive separately, each in order, with
// nothing added.
func caseExecStreams(c *C) {
	i := c.Shared()
	token := randHex(4)
	r := c.Exec(i, ShArgv(`printf 'out-%s' "$1"; printf 'err-%s' "$1" >&2`, token), nil)
	if r.Err != nil || string(r.Stdout) != "out-"+token || string(r.Stderr) != "err-"+token {
		c.Errorf("want stdout %q and stderr %q exactly, got %s", "out-"+token, "err-"+token, r)
	}

	r = c.Exec(i, ShArgv(`i=1; while [ $i -le 300 ]; do echo o$i; echo e$i >&2; i=$((i+1)); done`), nil)
	var wantOut, wantErr strings.Builder
	for n := 1; n <= 300; n++ {
		fmt.Fprintf(&wantOut, "o%d\n", n)
		fmt.Fprintf(&wantErr, "e%d\n", n)
	}
	if r.Err != nil || string(r.Stdout) != wantOut.String() || string(r.Stderr) != wantErr.String() {
		c.Errorf("300 interleaved lines on each stream did not arrive apart and in order: %s", r)
	}
}

// caseExecStdin: stdin reaches the command byte for byte and ends with EOF; a
// command given no stdin sees EOF at once; a command keeps running after its
// stdin ends.
func caseExecStdin(c *C) {
	i := c.Shared()
	payload := randomBytes(1<<20, 1)
	if r := c.Exec(i, []string{"cat"}, bytes.NewReader(payload)); r.Err != nil || !bytes.Equal(r.Stdout, payload) {
		c.Errorf("1 MiB of binary through cat came back different (%d bytes): %s", len(r.Stdout), r)
	}
	if r := c.Exec(i, ShArgv("wc -c"), bytes.NewReader(randomBytes(3<<20, 2))); r.Err != nil || strings.TrimSpace(string(r.Stdout)) != "3145728" {
		c.Errorf("wc -c of 3 MiB: %s", r)
	}
	start := time.Now()
	if r := c.Exec(i, []string{"cat"}, nil); r.Err != nil || len(r.Stdout) != 0 {
		c.Errorf("cat with no stdin: want an immediate EOF and no output, got %s", r)
	} else if d := time.Since(start); d > time.Minute {
		c.Errorf("cat with no stdin took %s; stdin did not end", d)
	}
	if r := c.Exec(i, ShArgv("cat >/dev/null; echo after-eof"), strings.NewReader("x\n")); r.Err != nil || string(r.Stdout) != "after-eof\n" {
		c.Errorf("a command reading stdin to EOF then going on: %s", r)
	}
}

// caseExecUser: every exec path runs as one user, the UpResult's RemoteUser:
// ExecCommand, ExecCommandQuiet and RunScript (which the session poller and
// automation launches use). A session one starts is then visible to the
// others.
func caseExecUser(c *C) {
	i := c.Shared()
	if got := c.Sh(i, "id -un"); got != i.Result.RemoteUser {
		c.Errorf("ExecCommand runs as %q, want UpResult.RemoteUser %q", got, i.Result.RemoteUser)
	}
	uid := c.Sh(i, "id -u")
	if r := c.ExecQuiet(i, ShArgv("id -u"), nil); strings.TrimSpace(string(r.Stdout)) != uid {
		c.Errorf("ExecCommandQuiet runs as uid %q, ExecCommand as %q: %s", r.Stdout, uid, r)
	}
	if got := c.Script(i, "id -u"); got != uid {
		c.Errorf("RunScript runs as uid %q, ExecCommand as %q: sessions one starts are invisible to the other", got, uid)
	}
	if got := c.Script(i, `printf '%s' "$HOME"`); got != c.Home(i) {
		c.Errorf("RunScript's $HOME is %q, ExecCommand's %q", got, c.Home(i))
	}
}

// caseExecQuiet: ExecCommandQuiet behaves like ExecCommand but writes no
// event-log entry; ExecCommand writes one "container exec" entry when run
// through its Run/Output methods.
func caseExecQuiet(c *C) {
	i := c.Shared()
	marker := "conformance-quiet-" + randHex(6)
	logSize := func() int64 {
		info, err := os.Stat(c.Target().EventLog)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	// logSlice is the log's bytes in [from, to); to < 0 means to the end.
	logSlice := func(from, to int64) string {
		data, err := os.ReadFile(c.Target().EventLog)
		if err != nil {
			return ""
		}
		if to < 0 || to > int64(len(data)) {
			to = int64(len(data))
		}
		if from > to {
			return ""
		}
		return string(data[from:to])
	}

	before := logSize()
	quietMarker, loudMarker := marker+"-q", marker+"-l"
	quiet, err := i.B.ExecCommandQuiet(i.WorkspaceDir(), []string{"echo", quietMarker}).Output()
	if err != nil || string(quiet) != quietMarker+"\n" {
		c.Errorf("ExecCommandQuiet output %q (err %v), want %q", quiet, err, quietMarker)
	}
	afterQuiet := logSize()
	loud, err := i.B.ExecCommand(i.WorkspaceDir(), []string{"echo", loudMarker}).Output()
	if err != nil || string(loud) != loudMarker+"\n" {
		c.Errorf("ExecCommand output %q (err %v), want %q", loud, err, loudMarker)
	}

	if c.Target().EventLog == "" {
		c.Logf("no event log to inspect on %s; checked the output only", c.Target().Name)
		return
	}
	if got := logSlice(before, afterQuiet); strings.Contains(got, quietMarker) {
		c.Errorf("ExecCommandQuiet wrote an event-log entry:\n%s", got)
	}
	if got := logSlice(afterQuiet, -1); !strings.Contains(got, "container exec") || !strings.Contains(got, loudMarker) {
		c.Errorf("ExecCommand wrote no \"container exec\" entry naming its command to %s; new log:\n%s", c.Target().EventLog, got)
	}
}

// caseExecSync: Exec runs a command to completion before returning and
// returns its exit status.
func caseExecSync(c *C) {
	i := c.Shared()
	if err := i.B.Exec(i.WorkspaceDir(), []string{"true"}); err != nil {
		c.Errorf("Exec true: %v", err)
	}
	if err := i.B.Exec(i.WorkspaceDir(), ShArgv("exit 3")); exitCode(err) != 3 {
		c.Errorf("Exec exit 3 returned %v, want exit status 3", err)
	}
	marker := c.Scratch(i) + "/exec-sync"
	if err := i.B.Exec(i.WorkspaceDir(), ShArgv(`sleep 1; : > "$1"`, marker)); err != nil {
		c.Fatalf("Exec: %v", err)
	}
	if r := c.Exec(i, []string{"test", "-e", marker}, nil); r.Err != nil {
		c.Errorf("Exec returned before its command finished: %s is missing", marker)
	}
}
