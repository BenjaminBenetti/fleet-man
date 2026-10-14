// Package sshserver is `fleet ssh-server --stdio`: an SSH server for exactly
// one connection, carried on its own stdin and stdout, that VS Code
// Remote-SSH reaches a fleet instance through. OpenSSH on the editor's
// machine runs `fleet ssh-proxy <alias>` (sshalias), which opens the daemon's
// Exec stream running this command inside the instance as the
// devcontainer's remoteUser. So the server needs no listener, no port and no
// user authentication: whoever can open that Exec stream already controls
// the instance. It has no listen mode, and nothing here can make one.
//
// It serves sessions (exec, shell, pty, env, signals, the sftp subsystem),
// direct-tcpip and direct-streamlocal forwards (how VS Code reaches its
// server and auto-forwards ports), and nothing else. Its host key is a fresh
// ed25519 key per run. Sessions inherit its environment, so SSH_AUTH_SOCK is
// fleet's relay; agent forwarding requests are refused.
//
// It also applies the devcontainer's customizations.vscode the way the Dev
// Containers extension does on attach (vscode.go): once VS Code's server
// runs in the instance it installs the extensions and merges the machine
// settings, and on each VS Code connection it runs postAttachCommand.
//
// BOUNDARY: in-instance code, held to depguard's client rule.
package sshserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Options configure one connection.
type Options struct {
	// Log receives the server's report lines (what it applied for VS Code,
	// postAttachCommand's output, failures). The daemon copies them into the
	// instance's creation log. Nil discards them.
	Log io.Writer
	// Environ is the sessions' base environment (default os.Environ()).
	Environ []string
	// User is whom sessions run as (default: the current user).
	User *Account
	// EditorConfig is where the EditorConfig is read (default
	// devmeta.EditorConfigPath); VSCode tunes the customizations (nil:
	// defaults; Disabled: none).
	EditorConfig string
	VSCode       *VSCodeOptions
}

// Serve runs the SSH server for one connection on in and out (the
// command's stdin and stdout) until the client disconnects or ctx ends.
func Serve(ctx context.Context, in io.Reader, out io.Writer, opts Options) error {
	if opts.Environ == nil {
		opts.Environ = os.Environ()
	}
	if opts.User == nil {
		acct, err := CurrentAccount()
		if err != nil {
			return err
		}
		opts.User = acct
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	rep := &reporter{w: opts.Log}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true, ServerVersion: "SSH-2.0-fleet"}
	cfg.AddHostKey(signer)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := &stdioConn{in: in, out: out, closed: make(chan struct{})}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil // the client went before the handshake
		}
		return fmt.Errorf("ssh handshake: %w", err)
	}
	defer sconn.Close()

	c := &connection{opts: opts, rep: rep, ctx: ctx}
	c.vscode = newVSCodeWatch(ctx, c)
	go ssh.DiscardRequests(reqs) // keepalives answered false; no remote forwards
	var wg sync.WaitGroup
	for nc := range chans {
		wg.Add(1)
		go func(nc ssh.NewChannel) {
			defer wg.Done()
			c.handleChannel(nc)
		}(nc)
	}
	cancel()
	wg.Wait()
	c.vscode.wait()
	return nil
}

// connection is one client's state.
type connection struct {
	opts   Options
	rep    *reporter
	ctx    context.Context
	vscode *vscodeWatch
}

func (c *connection) handleChannel(nc ssh.NewChannel) {
	switch nc.ChannelType() {
	case "session":
		c.handleSession(nc)
	case "direct-tcpip":
		c.handleDirectTCPIP(nc)
	case "direct-streamlocal@openssh.com":
		c.handleDirectStreamLocal(nc)
	default:
		_ = nc.Reject(ssh.UnknownChannelType, "unsupported channel type "+nc.ChannelType())
	}
}

// reporter writes whole report lines from any goroutine.
type reporter struct {
	mu sync.Mutex
	w  io.Writer
}

func (r *reporter) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = io.WriteString(r.w, line+"\n")
}

// writer is an io.Writer of whole lines with prefix, for a command's
// output.
func (r *reporter) writer(prefix string) *lineWriter { return &lineWriter{r: r, prefix: prefix} }

type lineWriter struct {
	r      *reporter
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.r.printf("%s%s", w.prefix, w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes what is left of an unfinished line.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.r.printf("%s%s", w.prefix, w.buf)
		w.buf = nil
	}
}

// stdioConn is the connection as the SSH library wants it: a net.Conn whose
// reads are stdin and whose writes are stdout.
type stdioConn struct {
	in     io.Reader
	out    io.Writer
	once   sync.Once
	closed chan struct{}
}

func (c *stdioConn) Read(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.EOF
	default:
	}
	return c.in.Read(p)
}

func (c *stdioConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	return c.out.Write(p)
}

// Close closes stdout (the client sees EOF) and stdin where they can be.
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		if cl, ok := c.out.(io.Closer); ok {
			_ = cl.Close()
		}
		if cl, ok := c.in.(io.Closer); ok {
			_ = cl.Close()
		}
	})
	return nil
}

func (c *stdioConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return stdioAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }
func (stdioAddr) Network() string                     { return "stdio" }
func (stdioAddr) String() string                      { return "stdio" }

type stdioAddr struct{}
