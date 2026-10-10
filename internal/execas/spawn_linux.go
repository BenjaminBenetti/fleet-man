//go:build linux

package execas

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// AbortGrace is how long Abort waits after its hangup or SIGTERM before it
// kills whatever is left of the command's session.
const AbortGrace = 5 * time.Second

// abortGrace is AbortGrace, shortened by tests.
var abortGrace = AbortGrace

// CanSwitchUser reports whether this process may run commands as other users:
// it is root with CAP_SETUID and CAP_SETGID. False means capability tier C,
// single-uid mode; the agent reports it as Hello's can_setuid.
func CanSwitchUser() bool {
	if os.Geteuid() != 0 {
		return false
	}
	caps, err := effectiveCaps()
	if err != nil {
		return false
	}
	return caps&(1<<unix.CAP_SETUID) != 0 && caps&(1<<unix.CAP_SETGID) != 0
}

// effectiveCaps reads this process's effective capability set.
func effectiveCaps() (uint64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		}
	}
	return 0, errors.New("no CapEff in /proc/self/status")
}

// CurrentCreds are this process's own uid, gid and supplementary groups.
func CurrentCreds() Creds {
	creds := Creds{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	groups, _ := os.Getgroups()
	for _, g := range groups {
		creds.Groups = append(creds.Groups, uint32(g))
	}
	return creds
}

// Stdio is a command's standard streams when it has no TTY. A nil stream is
// /dev/null. When a stream is not an *os.File, a goroutine copies it, and
// WaitDelay bounds how long Wait waits for that copy after the command exits
// (a background child can hold the pipe open); 0 waits for it to finish.
type Stdio struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	WaitDelay time.Duration
}

// Process is a command Start started, in a session of its own.
type Process struct {
	cmd *exec.Cmd
	pty *os.File

	abortOnce sync.Once
}

// Start starts plan's command as a child of this process, in a new session
// whose id is its pid, so Abort and Kill reach everything it spawns. With a
// TTY it runs on a fresh PTY, its controlling terminal, chowned to its user
// and the tty group with mode 0620 as docker does (left as opened in
// single-uid mode, where it is this process's already); stdio is then unused
// and the caller reads and writes PTY(). A failure to start is an *ExitError.
func Start(plan *Plan, stdio Stdio) (*Process, error) {
	// A missing directory fails in the child as ENOENT, which would read as a
	// missing executable; name it here instead.
	if fi, err := os.Stat(plan.Dir); err != nil {
		return nil, cannotRun(fmt.Errorf("chdir %s: %w", plan.Dir, unwrapPath(err)))
	} else if !fi.IsDir() {
		return nil, cannotRun(fmt.Errorf("chdir %s: %w", plan.Dir, syscall.ENOTDIR))
	}
	cmd := &exec.Cmd{Path: plan.Path, Args: plan.Argv, Env: plan.Env, Dir: plan.Dir, WaitDelay: stdio.WaitDelay}
	attr := &syscall.SysProcAttr{Setsid: true}
	if plan.Switch {
		attr.Credential = &syscall.Credential{
			Uid:    plan.Identity.UID,
			Gid:    plan.Identity.GID,
			Groups: plan.Identity.Groups,
		}
	}
	cmd.SysProcAttr = attr

	var master *os.File
	if plan.TTY {
		m, tty, err := openTTY(plan)
		if err != nil {
			return nil, failed(err)
		}
		defer tty.Close()
		master = m
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		attr.Setctty = true
		attr.Ctty = 0
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	}

	if err := cmd.Start(); err != nil {
		if master != nil {
			_ = master.Close()
		}
		return nil, startError(plan, err)
	}
	return &Process{cmd: cmd, pty: master}, nil
}

// openTTY opens a PTY sized for plan, its slave owned by plan's user.
func openTTY(plan *Plan) (master, tty *os.File, err error) {
	master, tty, err = pty.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("open pty: %w", err)
	}
	defer func() {
		if err != nil {
			_ = master.Close()
			_ = tty.Close()
		}
	}()
	if plan.Rows > 0 || plan.Cols > 0 {
		if err := pty.Setsize(master, &pty.Winsize{Rows: plan.Rows, Cols: plan.Cols}); err != nil {
			return nil, nil, fmt.Errorf("size pty: %w", err)
		}
	}
	if plan.Switch {
		if err := tty.Chown(int(plan.Identity.UID), int(plan.TTYGroup)); err != nil {
			return nil, nil, fmt.Errorf("chown pty: %w", err)
		}
		if err := tty.Chmod(0o620); err != nil {
			return nil, nil, fmt.Errorf("chmod pty: %w", err)
		}
	}
	return master, tty, nil
}

// startError turns a failed start into docker's exit code: 127 when the
// executable (or its interpreter) is missing, 126 for anything else that kept
// it from running (permissions, the working directory as the user, the user).
func startError(plan *Plan, err error) error {
	if errors.Is(err, syscall.ENOENT) {
		return notFound(fmt.Errorf("exec %s: %w", plan.Argv[0], syscall.ENOENT))
	}
	return cannotRun(fmt.Errorf("exec %s: %w", plan.Argv[0], unwrapPath(err)))
}

func unwrapPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// Pid is the command's pid, which is also its session and process group id.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// PTY is the PTY's master end with a TTY, nil without one. The caller owns it:
// it reads the command's output from it until it fails, writes input to it,
// and closes it when done.
func (p *Process) PTY() *os.File { return p.pty }

// Resize sets the TTY's window size; without one it does nothing.
func (p *Process) Resize(rows, cols uint16) error {
	if p.pty == nil {
		return nil
	}
	return pty.Setsize(p.pty, &pty.Winsize{Rows: rows, Cols: cols})
}

// Wait waits for the command to exit and returns how it ended. err is non-nil
// only when no exit status could be had, or copying a non-file stream failed.
// Call it exactly once.
func (p *Process) Wait() (ExitStatus, error) {
	err := p.cmd.Wait()
	state := p.cmd.ProcessState
	if state == nil {
		return ExitStatus{Code: -1}, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		err = nil
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Code: -1, Signal: ws.Signal()}, err
	}
	return ExitStatus{Code: state.ExitCode()}, err
}

// Signal sends sig to the command's foreground: with a TTY, the terminal's
// foreground process group (the job an interactive shell is running, as a
// keypress would), else the command's own process group.
func (p *Process) Signal(sig syscall.Signal) error {
	target := p.Pid()
	if p.pty != nil {
		if pgrp, err := foregroundGroup(p.pty); err == nil && pgrp > 0 {
			target = pgrp
		}
	}
	return ignoreGone(syscall.Kill(-target, sig))
}

// foregroundGroup asks the terminal behind master for its foreground process
// group, without putting master into blocking mode as Fd would.
func foregroundGroup(master *os.File) (int, error) {
	conn, err := master.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgrp int
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		pgrp, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return 0, err
	}
	return pgrp, ioctlErr
}

// Abort ends the command: it hangs up the TTY (closing PTY(), so the kernel
// sends SIGHUP to the session, and an interactive shell passes it on to its
// jobs) or sends SIGTERM to the process group, then after AbortGrace kills
// every process still in the command's session. It returns at once, may be
// called more than once and from any goroutine, and Wait must still be called.
func (p *Process) Abort() {
	p.abortOnce.Do(func() {
		if p.pty != nil {
			_ = p.pty.Close()
			_ = syscall.Kill(-p.Pid(), syscall.SIGHUP)
		} else {
			_ = syscall.Kill(-p.Pid(), syscall.SIGTERM)
		}
		sid := p.Pid()
		time.AfterFunc(abortGrace, func() { killSession(sid, syscall.SIGKILL) })
	})
}

// Kill sends SIGKILL to every process in the command's session now.
func (p *Process) Kill() {
	killSession(p.Pid(), syscall.SIGKILL)
}

// killSession signals the process group sid and every other process whose
// session is sid (a job an interactive shell moved to a group of its own).
// Processes that started a session of their own (daemons) are out of reach, as
// they are of docker.
func killSession(sid int, sig syscall.Signal) {
	_ = syscall.Kill(-sid, sig)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil {
			continue
		}
		if s, ok := sessionOf(pid); ok && s == sid {
			_ = syscall.Kill(pid, sig)
		}
	}
}

// sessionOf reads a process's session id from /proc/<pid>/stat: the fourth
// field after the command name, which is parenthesized and may hold anything.
func sessionOf(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 4 {
		return 0, false
	}
	sid, err := strconv.Atoi(fields[3])
	return sid, err == nil
}

// isTerminal reports whether f is a terminal, without putting it into
// blocking mode as Fd would.
func isTerminal(f *os.File) bool {
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		_, ioctlErr = unix.IoctlGetTermios(int(fd), unix.TCGETS)
	}); err != nil {
		return false
	}
	return ioctlErr == nil
}

func ignoreGone(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Exec runs plan's command in place of this process: it takes the user's
// groups, gid and uid, moves to the working directory as that user, and execs.
// On success it does not return, and stdin, stdout, stderr and the exit status
// are the command's own. It takes no TTY; a TTY needs Start. A failure is an
// *ExitError, and once the user is taken this process cannot get root back, so
// the caller must only report it and exit.
func Exec(plan *Plan) error {
	if plan.TTY {
		return failed(errors.New("a tty command needs a process to serve its terminal; exec it through the pod agent"))
	}
	if plan.Switch {
		groups := make([]int, len(plan.Identity.Groups))
		for i, g := range plan.Identity.Groups {
			groups[i] = int(g)
		}
		if err := syscall.Setgroups(groups); err != nil {
			return cannotRun(fmt.Errorf("setgroups: %w", err))
		}
		if err := syscall.Setgid(int(plan.Identity.GID)); err != nil {
			return cannotRun(fmt.Errorf("setgid %d: %w", plan.Identity.GID, err))
		}
		if err := syscall.Setuid(int(plan.Identity.UID)); err != nil {
			return cannotRun(fmt.Errorf("setuid %d: %w", plan.Identity.UID, err))
		}
	}
	if err := syscall.Chdir(plan.Dir); err != nil {
		return cannotRun(fmt.Errorf("chdir %s: %w", plan.Dir, err))
	}
	err := syscall.Exec(plan.Path, plan.Argv, plan.Env)
	if errors.Is(err, syscall.ENOENT) {
		return notFound(fmt.Errorf("exec %s: %w", plan.Argv[0], err))
	}
	return cannotRun(fmt.Errorf("exec %s: %w", plan.Argv[0], err))
}
