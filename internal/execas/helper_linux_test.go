//go:build linux

package execas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// helperReport is what the "report" helper prints about itself: the identity,
// environment and terminal a started command actually got.
type helperReport struct {
	UID    int
	GID    int
	EUID   int
	Groups []int
	Cwd    string
	Env    []string
	Args   []string
	Sid    int
	Pid    int
	Stdin  string     `json:",omitempty"`
	TTY    *ttyReport `json:",omitempty"`
}

type ttyReport struct {
	UID  uint32
	GID  uint32
	Mode uint32
	Rows uint16
	Cols uint16
	// Ctty is whether /dev/tty opens: the PTY is the controlling terminal.
	Ctty bool
}

// runTestHelper plays a helper role and exits:
//
//	report   print a helperReport between <<< and >>>
//	         (EXECAS_TEST_STDIN=1 reads stdin into it first)
//	tree     start a sleep in a process group of its own, print its pid, sleep
//	exec-as  be `fleet exec-as` (EXECAS_TEST_SPEC, EXECAS_TEST_CACHE)
func runTestHelper(role string) {
	switch role {
	case "report":
		writeReport()
		os.Exit(0)
	case "tree":
		child := exec.Command("sleep", "300")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Printf("GRANDCHILD %d\n", child.Process.Pid)
		time.Sleep(300 * time.Second)
		os.Exit(0)
	case "exec-as":
		err := RunExecAs(context.Background(), os.Stdin, ExecAsOptions{
			SpecPath: os.Getenv("EXECAS_TEST_SPEC"),
			CacheDir: os.Getenv("EXECAS_TEST_CACHE"),
		})
		fmt.Fprintf(os.Stderr, "fleet exec-as: %v\n", err)
		os.Exit(ExitCode(err))
	}
}

func writeReport() {
	r := helperReport{
		UID:  os.Getuid(),
		GID:  os.Getgid(),
		EUID: os.Geteuid(),
		Env:  os.Environ(),
		Args: os.Args,
		Pid:  os.Getpid(),
	}
	r.Groups, _ = os.Getgroups()
	r.Cwd, _ = os.Getwd()
	r.Sid, _ = sessionOf(os.Getpid())
	if os.Getenv("EXECAS_TEST_STDIN") == "1" {
		data, _ := io.ReadAll(os.Stdin)
		r.Stdin = string(data)
	}
	if isTerminal(os.Stdin) {
		var st syscall.Stat_t
		_ = syscall.Fstat(0, &st)
		tr := &ttyReport{UID: st.Uid, GID: st.Gid, Mode: st.Mode & 0o777}
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
			tr.Rows, tr.Cols = ws.Rows, ws.Cols
		}
		if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
			tr.Ctty = true
			f.Close()
		}
		r.TTY = tr
	}
	data, _ := json.Marshal(r)
	fmt.Printf("<<<%s>>>\n", data)
}

// parseReport finds the helper's report in out.
func parseReport(t *testing.T, out []byte) helperReport {
	t.Helper()
	start := bytes.Index(out, []byte("<<<"))
	end := bytes.LastIndex(out, []byte(">>>"))
	if start < 0 || end < start {
		t.Fatalf("no report in output: %q", out)
	}
	var r helperReport
	if err := json.Unmarshal(out[start+3:end], &r); err != nil {
		t.Fatalf("parse report %q: %v", out[start+3:end], err)
	}
	return r
}

// helperBinary is this test binary, which the helper roles run as. A test
// that runs it as another user copies it somewhere that user can reach.
func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// publicCopy copies the test binary into a directory every user may traverse
// and execute from, for helpers run as other uids.
func publicCopy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for d := dir; d != os.TempDir() && d != "/" && d != "."; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o755); err != nil {
			break
		}
	}
	data, err := os.ReadFile(helperBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "execas.test")
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// processGone reports whether pid has exited (gone or a zombie nobody reaped).
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(data, ')')
	return i >= 0 && len(data) > i+2 && data[i+2] == 'Z'
}

// waitGone waits up to d for pid to exit.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

// selfRoot is a container /etc whose only entry is this test's own user, with
// home home, plus extra passwd and group lines.
func selfRoot(t *testing.T, home, passwd, group string) string {
	t.Helper()
	me := CurrentCreds()
	line := fmt.Sprintf("me:x:%d:%d::%s:/bin/sh\n", me.UID, me.GID, home)
	return writeRoot(t, line+passwd, group)
}

// envHas reports whether env holds exactly the entries want, in any order.
func sameEnv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int)
	for _, e := range a {
		seen[e]++
	}
	for _, e := range b {
		seen[e]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func hasEnv(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }
