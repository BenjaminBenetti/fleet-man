package sshserver

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// session.go serves "session" channels the way sshd does for a client: the
// login shell for a shell request, `<shell> -c <command>` for an exec, the
// sftp subsystem, on a pty when one was asked for, in the account's home,
// with the account's environment plus what the client sent. The exit
// status (or the killing signal) goes back before the channel closes.

// ptyDrain is how long a pty's output is still read after its command
// exited, for what a background job holding the terminal keeps writing.
const ptyDrain = 2 * time.Second

type ptyRequest struct {
	Term          string
	Cols, Rows    uint32
	Width, Height uint32
	Modes         string
}

type session struct {
	c  *connection
	ch ssh.Channel

	mu      sync.Mutex
	env     []string
	pty     *ptyRequest
	ptmx    *os.File
	started bool
	proc    *os.Process
}

func (c *connection) handleSession(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	s := &session{c: c, ch: ch}
	for req := range reqs {
		ok := s.request(req)
		if req.WantReply {
			_ = req.Reply(ok, nil)
		}
	}
	// The client closed the channel. As with sshd, a terminal's command
	// gets the hangup of its terminal; a command on pipes reads EOF and has
	// its writes fail, and is not signalled (what it left running in the
	// background, such as VS Code's server, keeps running).
	s.closePTY()
}

// closePTY closes the session's terminal, once; under the lock, so a
// window change never sizes a closed one.
func (s *session) closePTY() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx != nil {
		_ = s.ptmx.Close()
		s.ptmx = nil
	}
}

// request handles one channel request; its result is the reply.
func (s *session) request(req *ssh.Request) bool {
	switch req.Type {
	case "env":
		var kv struct{ Name, Value string }
		if ssh.Unmarshal(req.Payload, &kv) != nil || kv.Name == "" || strings.ContainsAny(kv.Name, "=\x00") || strings.ContainsRune(kv.Value, 0) {
			return false
		}
		s.mu.Lock()
		s.env = append(s.env, kv.Name+"="+kv.Value)
		s.mu.Unlock()
		return true
	case "pty-req":
		var p ptyRequest
		if ssh.Unmarshal(req.Payload, &p) != nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.started {
			return false
		}
		s.pty = &p
		return true
	case "window-change":
		var w struct{ Cols, Rows, Width, Height uint32 }
		if ssh.Unmarshal(req.Payload, &w) != nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pty != nil {
			s.pty.Cols, s.pty.Rows = w.Cols, w.Rows
		}
		if s.ptmx != nil {
			_ = pty.Setsize(s.ptmx, winsize(w.Cols, w.Rows))
		}
		return true
	case "shell":
		acct := s.c.opts.User
		return s.start(acct.Shell, []string{"-" + filepath.Base(acct.Shell)})
	case "exec":
		var cmd struct{ Command string }
		if ssh.Unmarshal(req.Payload, &cmd) != nil {
			return false
		}
		acct := s.c.opts.User
		return s.start(acct.Shell, []string{acct.Shell, "-c", cmd.Command})
	case "subsystem":
		var sub struct{ Name string }
		if ssh.Unmarshal(req.Payload, &sub) != nil || sub.Name != "sftp" {
			return false
		}
		return s.startSFTP()
	case "signal":
		var sig struct{ Signal string }
		if ssh.Unmarshal(req.Payload, &sig) != nil {
			return false
		}
		if n, ok := signals[sig.Signal]; ok {
			s.signal(n)
		}
		return true
	default:
		// auth-agent-req@openssh.com among them: sessions keep fleet's own
		// SSH_AUTH_SOCK relay.
		return false
	}
}

// environ is a session's environment: the server's, the account's, the
// terminal's, then what the client sent (later entries win).
func (s *session) environ() []string {
	acct := s.c.opts.User
	drop := map[string]bool{"USER": true, "LOGNAME": true, "HOME": true, "SHELL": true, "PWD": true, "TERM": true}
	env := make([]string, 0, len(s.c.opts.Environ)+8)
	for _, kv := range s.c.opts.Environ {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			env = append(env, kv)
		}
	}
	env = append(env, "USER="+acct.Name, "LOGNAME="+acct.Name, "HOME="+acct.Home, "SHELL="+acct.Shell, "PWD="+acct.Home)
	if s.pty != nil && s.pty.Term != "" {
		env = append(env, "TERM="+s.pty.Term)
	}
	return append(env, s.env...)
}

// claim marks the session started; a session runs one thing.
func (s *session) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return false
	}
	s.started = true
	return true
}

// start runs path with args (args[0] is the process's name) on the
// session's pty or pipes.
func (s *session) start(path string, args []string) bool {
	if !s.claim() {
		return false
	}
	cmd := &exec.Cmd{Path: path, Args: args, Dir: s.c.opts.User.Home, Env: s.environ()}
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/"
	}
	s.mu.Lock()
	tty := s.pty
	s.mu.Unlock()
	if tty != nil {
		return s.startPTY(cmd, tty)
	}
	return s.startPipes(cmd)
}

func (s *session) startPTY(cmd *exec.Cmd, tty *ptyRequest) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	ptmx, err := pty.StartWithAttrs(cmd, winsize(tty.Cols, tty.Rows), cmd.SysProcAttr)
	if err != nil {
		s.failStart(err)
		return true
	}
	s.mu.Lock()
	s.ptmx, s.proc = ptmx, cmd.Process
	s.mu.Unlock()
	go func() { _, _ = io.Copy(ptmx, s.ch) }()
	out := make(chan struct{})
	go func() {
		_, _ = io.Copy(s.ch, ptmx)
		close(out)
	}()
	go func() {
		_ = cmd.Wait()
		select {
		case <-out:
		case <-time.After(ptyDrain):
		}
		s.closePTY()
		<-out
		s.finish(cmd.ProcessState)
	}()
	return true
}

func (s *session) startPipes(cmd *exec.Cmd) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.failStart(err)
		return true
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.failStart(err)
		return true
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.failStart(err)
		return true
	}
	if err := cmd.Start(); err != nil {
		s.failStart(err)
		return true
	}
	s.mu.Lock()
	s.proc = cmd.Process
	s.mu.Unlock()
	go func() {
		// The client's EOF is the command's.
		_, _ = io.Copy(stdin, s.ch)
		_ = stdin.Close()
	}()
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		// A copy that fails (the client went) closes its pipe, so the
		// command's next write fails rather than blocking forever.
		go func() { defer wg.Done(); copyOrClose(s.ch, stdout) }()
		go func() { defer wg.Done(); copyOrClose(s.ch.Stderr(), stderr) }()
		wg.Wait()
		_ = cmd.Wait()
		s.finish(cmd.ProcessState)
	}()
	return true
}

func copyOrClose(dst io.Writer, src io.ReadCloser) {
	if _, err := io.Copy(dst, src); err != nil {
		_ = src.Close()
	}
}

// startSFTP serves the sftp subsystem on the channel, rooted at home.
func (s *session) startSFTP() bool {
	if !s.claim() {
		return false
	}
	srv, err := sftp.NewServer(s.ch, sftp.WithServerWorkingDirectory(s.c.opts.User.Home))
	if err != nil {
		s.failStart(err)
		return true
	}
	go func() {
		err := srv.Serve()
		_ = srv.Close()
		code := uint32(0)
		if err != nil && !errors.Is(err, io.EOF) {
			code = 1
		}
		s.sendExit(code)
	}()
	return true
}

// failStart reports a command that could not start, as sshd does, with
// exit status 127.
func (s *session) failStart(err error) {
	_, _ = io.WriteString(s.ch.Stderr(), "fleet ssh-server: "+err.Error()+"\n")
	s.sendExit(127)
}

// finish sends the command's exit status, or the signal that killed it,
// and closes the channel.
func (s *session) finish(st *os.ProcessState) {
	if st == nil {
		s.sendExit(255)
		return
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := signalName(ws.Signal())
		_ = s.ch.CloseWrite()
		_, _ = s.ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Error      string
			Lang       string
		}{Signal: name, CoreDumped: ws.CoreDump()}))
		_ = s.ch.Close()
		return
	}
	s.sendExit(uint32(st.ExitCode()))
}

func (s *session) sendExit(code uint32) {
	_ = s.ch.CloseWrite()
	_, _ = s.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	_ = s.ch.Close()
}

// signal sends sig to the session's process group.
func (s *session) signal(sig syscall.Signal) {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, sig); err != nil {
		_ = proc.Signal(sig)
	}
}

func winsize(cols, rows uint32) *pty.Winsize {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return &pty.Winsize{Cols: uint16(min(cols, 0xffff)), Rows: uint16(min(rows, 0xffff))}
}

// signals are the signal names of RFC 4254 6.10.
var signals = map[string]syscall.Signal{
	"ABRT": syscall.SIGABRT, "ALRM": syscall.SIGALRM, "FPE": syscall.SIGFPE, "HUP": syscall.SIGHUP,
	"ILL": syscall.SIGILL, "INT": syscall.SIGINT, "KILL": syscall.SIGKILL, "PIPE": syscall.SIGPIPE,
	"QUIT": syscall.SIGQUIT, "SEGV": syscall.SIGSEGV, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

func signalName(sig syscall.Signal) string {
	for name, n := range signals {
		if n == sig {
			return name
		}
	}
	return "TERM"
}
