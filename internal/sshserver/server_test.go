package sshserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// pipeConn is the client's end of a stdio pair: OS pipes, buffered like
// the real stdin and stdout.
type pipeConn struct {
	r *os.File
	w *os.File
}

func (c pipeConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (c pipeConn) Write(p []byte) (int, error)    { return c.w.Write(p) }
func (c pipeConn) Close() error                   { _ = c.w.Close(); return c.r.Close() }
func (pipeConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (pipeConn) RemoteAddr() net.Addr             { return stdioAddr{} }
func (pipeConn) SetDeadline(time.Time) error      { return nil }
func (pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (pipeConn) SetWriteDeadline(time.Time) error { return nil }

// syncBuffer is a log the server and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rig is a home of its own and the options a test's connections share.
type rig struct {
	t    *testing.T
	home string
	log  *syncBuffer
	opts Options
}

func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	r := &rig{t: t, home: home, log: &syncBuffer{}}
	r.opts = Options{
		Log:          r.log,
		Environ:      []string{"PATH=" + os.Getenv("PATH"), "FROM_SERVER=1", "HOME=/nowhere"},
		User:         &Account{Name: "tester", Home: home, Shell: "/bin/sh"},
		EditorConfig: filepath.Join(t.TempDir(), "editor.json"),
		VSCode:       &VSCodeOptions{Disabled: true},
	}
	return r
}

// connect runs one server connection and returns a client on it; the
// connection ends with the test (or when done is called).
func (r *rig) connect() (client *ssh.Client, done func()) {
	r.t.Helper()
	c2sR, c2sW, err := os.Pipe()
	if err != nil {
		r.t.Fatal(err)
	}
	s2cR, s2cW, err := os.Pipe()
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, c2sR, s2cW, r.opts) }()
	conn := pipeConn{r: s2cR, w: c2sW}
	cc, chans, reqs, err := ssh.NewClientConn(conn, "alpha.repo.fleet", &ssh.ClientConfig{
		User:            "whoever",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		cancel()
		r.t.Fatalf("ssh handshake: %v", err)
	}
	client = ssh.NewClient(cc, chans, reqs)
	var once sync.Once
	done = func() {
		once.Do(func() {
			_ = client.Close()
			select {
			case err := <-served:
				if err != nil {
					r.t.Errorf("Serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				r.t.Error("Serve did not return after the client went")
			}
			cancel()
		})
	}
	r.t.Cleanup(done)
	return client, done
}

func run(t *testing.T, c *ssh.Client, setup func(*ssh.Session), command string) (stdout, stderr string, err error) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if setup != nil {
		setup(s)
	}
	var out, errOut bytes.Buffer
	s.Stdout, s.Stderr = &out, &errOut
	err = s.Run(command)
	return out.String(), errOut.String(), err
}

func TestExecRunsAsTheAccountWithItsEnvironment(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()

	out, errOut, err := run(t, c, nil, `echo out; echo err >&2; exit 3`)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || out != "out\n" || errOut != "err\n" {
		t.Fatalf("exec: %q %q %v", out, errOut, err)
	}

	out, _, err = run(t, c, func(s *ssh.Session) { _ = s.Setenv("FROM_CLIENT", "two words") },
		`printf '%s|%s|%s|%s|%s|' "$FROM_SERVER" "$FROM_CLIENT" "$HOME" "$USER" "$PWD"; pwd`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1|two words|" + r.home + "|tester|" + r.home + "|" + r.home + "\n"; out != want {
		t.Fatalf("environment %q, want %q", out, want)
	}

	// stdin, then EOF.
	s, _ := c.NewSession()
	s.Stdin = strings.NewReader("abc")
	got, err := s.Output("cat; echo -n :done")
	if err != nil || string(got) != "abc:done" {
		t.Fatalf("stdin: %q %v", got, err)
	}
}

func TestShellIsALoginShell(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Stdout = &out
	s.Stdin = strings.NewReader("echo \"$0\"; exit 4\n")
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	err = s.Wait()
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 4 || out.String() != "-sh\n" {
		t.Fatalf("shell: %q %v", out.String(), err)
	}
}

func TestPTYAndWindowChange(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	s.Stdout = &out
	stdin, _ := s.StdinPipe()
	if err := s.Start(`tty; echo "TERM=$TERM"; stty size; read line; stty size; echo "got $line"`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "30 100") })
	if err := s.WindowChange(40, 120); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, "hello\r")
	if err := s.Wait(); err != nil {
		t.Fatalf("pty session: %v (%q)", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"/dev/pts/", "TERM=xterm-256color", "30 100", "40 120", "got hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("pty output lacks %q:\n%s", want, got)
		}
	}
}

func TestSignalAndExitSignal(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	s.Stdout = &out
	if err := s.Start(`echo up; exec sleep 30`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return out.String() == "up\n" })
	if err := s.Signal(ssh.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = s.Wait()
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.Signal() != "TERM" {
		t.Fatalf("killed session: %v", err)
	}
}

func TestSFTP(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()
	fs, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	wd, err := fs.Getwd()
	if err != nil || wd != r.home {
		t.Fatalf("sftp working directory %q, %v", wd, err)
	}
	f, err := fs.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("over sftp")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	data, err := os.ReadFile(filepath.Join(r.home, "hello.txt"))
	if err != nil || string(data) != "over sftp" {
		t.Fatalf("written file %q, %v", data, err)
	}
	if err := fs.Rename("hello.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(".")
	if err != nil || len(entries) != 1 || entries[0].Name() != "moved.txt" {
		t.Fatalf("readdir %v %v", entries, err)
	}
}

// Forwards reach services inside the instance, loopback-only ones too, and
// a client's EOF is a half-close.
func TestDirectTCPIPAndStreamLocal(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	sock := filepath.Join(t.TempDir(), "s.sock")
	unix, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	// Each service reads to EOF, then answers with what it read.
	serve := func(l net.Listener) {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				data, _ := io.ReadAll(conn)
				_, _ = conn.Write(append([]byte("got:"), data...))
			}()
		}
	}
	go serve(tcp)
	go serve(unix)

	for _, tc := range []struct{ network, addr string }{{"tcp", tcp.Addr().String()}, {"unix", sock}} {
		conn, err := c.Dial(tc.network, tc.addr)
		if err != nil {
			t.Fatalf("dial %s %s: %v", tc.network, tc.addr, err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			t.Fatalf("%s forward has no half-close", tc.network)
		}
		got, err := io.ReadAll(conn)
		if err != nil || string(got) != "got:ping" {
			t.Errorf("%s forward: %q %v", tc.network, got, err)
		}
		_ = conn.Close()
	}
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Error("a forward to a closed port opened")
	}
}

// What the server does not offer it refuses: remote forwards, agent
// forwarding, X11, other subsystems and channel types.
func TestRefusals(t *testing.T) {
	r := newRig(t)
	c, _ := r.connect()
	if l, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		_ = l.Close()
		t.Error("a remote forward was opened")
	}
	s, _ := c.NewSession()
	if ok, err := s.SendRequest("auth-agent-req@openssh.com", true, nil); ok || err != nil {
		t.Errorf("agent forwarding: %v %v", ok, err)
	}
	if ok, _ := s.SendRequest("x11-req", true, ssh.Marshal(struct {
		Single bool
		Proto  string
		Cookie string
		Screen uint32
	}{false, "MIT-MAGIC-COOKIE-1", "00", 0})); ok {
		t.Error("x11 forwarding was accepted")
	}
	if err := s.RequestSubsystem("netconf"); err == nil {
		t.Error("an unknown subsystem was accepted")
	}
	_ = s.Close()
	if _, _, err := c.OpenChannel("tun@openssh.com", nil); err == nil {
		t.Error("an unknown channel type was accepted")
	}
	// A session runs one command.
	s, _ = c.NewSession()
	defer s.Close()
	if err := s.Start("sleep 2"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"true"})); ok {
		t.Error("a second command ran on one session")
	}
}

func TestCurrentAccountReadsTheLoginShell(t *testing.T) {
	passwd := filepath.Join(t.TempDir(), "passwd")
	uid := os.Getuid()
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\nme:x:"+strconv.Itoa(uid)+":"+strconv.Itoa(uid)+"::/home/me:/usr/bin/zsh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := passwdPath
	passwdPath = passwd
	t.Cleanup(func() { passwdPath = old })
	acct, err := CurrentAccount()
	if err != nil {
		t.Fatal(err)
	}
	if acct.Shell != "/usr/bin/zsh" || acct.Home == "" || acct.Name == "" {
		t.Fatalf("account %+v", acct)
	}
	passwdPath = filepath.Join(t.TempDir(), "none")
	t.Setenv("SHELL", "")
	if acct, err := CurrentAccount(); err != nil || acct.Shell != "/bin/sh" {
		t.Fatalf("no passwd entry: %+v %v", acct, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
