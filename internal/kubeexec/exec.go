//go:build !nok8s

package kubeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec protocols. A pod exec rides a WebSocket (the v5 channel protocol,
// which can half-close stdin), falling back to SPDY only when the API server
// or a proxy in between cannot upgrade to WebSocket. WebSocket comes first
// because containerd kills the process of an SPDY-carried exec whose stream
// is cut.
const (
	ProtocolAuto      = ""
	ProtocolWebSocket = "websocket"
	ProtocolSPDY      = "spdy"
)

// ExecRequest is one command to run in a pod's container.
type ExecRequest struct {
	Namespace string
	Pod       string
	Container string
	// Command is the argv, run without a shell. The API server logs it:
	// never put a secret or a script's variables in it.
	Command []string
	// Stdin attaches the command's stdin; TTY runs it on a terminal, whose
	// output merges stdout and stderr.
	Stdin bool
	TTY   bool
	// Protocol forces one exec protocol (ProtocolAuto: WebSocket, then SPDY).
	Protocol string
}

// TerminalSize is a TTY exec's window.
type TerminalSize struct {
	Rows, Cols uint16
}

// Stdio is an exec's streams. Stdin is read only when the request attaches
// it; Resize, when set, delivers a TTY's window sizes until it is closed.
type Stdio struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Resize <-chan TerminalSize
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return "command exited " + strconv.Itoa(e.Code) }

// Exec runs req in the connection's cluster and returns once it ends: nil
// for exit 0, an *ExitError for another exit code, or why the exec failed.
// ctx bounds the whole exec.
func (c *Connection) Exec(ctx context.Context, req ExecRequest, stdio Stdio) error {
	return ExecConfig(ctx, c.RESTConfig(), req, stdio)
}

// ExecConfig is Exec with a client configuration of the caller's own (the
// kube-exec shim's, built from an issued credential by RESTConfigFor).
func ExecConfig(ctx context.Context, cfg *rest.Config, req ExecRequest, stdio Stdio) error {
	if len(req.Command) == 0 {
		return errors.New("exec: no command")
	}
	u, err := execURL(cfg, req)
	if err != nil {
		return err
	}
	executor, err := newExecutor(cfg, u, req.Protocol)
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdout: stdio.Stdout, Tty: req.TTY}
	if req.Stdin {
		opts.Stdin = stdio.Stdin
	}
	if !req.TTY {
		opts.Stderr = stdio.Stderr
	}
	if req.TTY && stdio.Resize != nil {
		opts.TerminalSizeQueue = sizeQueue(stdio.Resize)
	}
	err = executor.StreamWithContext(ctx, opts)
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) {
		return &ExitError{Code: exit.ExitStatus()}
	}
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("exec %s/%s: %w", req.Namespace, req.Pod, ctx.Err())
	}
	return err
}

// execURL is the pods/exec URL of req.
func execURL(cfg *rest.Config, req ExecRequest) (*url.URL, error) {
	if req.Namespace == "" || req.Pod == "" {
		return nil, errors.New("exec: no namespace or pod")
	}
	core := *cfg
	core.APIPath = "/api"
	core.GroupVersion = &schema.GroupVersion{Version: "v1"}
	base, versioned, err := rest.DefaultServerUrlFor(&core)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	u := *base
	u.Path = path.Join("/", base.Path, versioned, "namespaces", req.Namespace, "pods", req.Pod, "exec")
	q := url.Values{}
	if req.Container != "" {
		q.Set("container", req.Container)
	}
	for _, arg := range req.Command {
		q.Add("command", arg)
	}
	q.Set("stdout", "true")
	if req.Stdin {
		q.Set("stdin", "true")
	}
	if req.TTY {
		q.Set("tty", "true")
	} else {
		q.Set("stderr", "true")
	}
	u.RawQuery = q.Encode()
	return &u, nil
}

func newExecutor(cfg *rest.Config, u *url.URL, protocol string) (remotecommand.Executor, error) {
	switch protocol {
	case ProtocolAuto, ProtocolWebSocket, ProtocolSPDY:
	default:
		return nil, fmt.Errorf("exec: unknown protocol %q", protocol)
	}
	var ws, spdy remotecommand.Executor
	var err error
	if protocol != ProtocolSPDY {
		if ws, err = remotecommand.NewWebSocketExecutor(cfg, "GET", u.String()); err != nil {
			return nil, fmt.Errorf("exec over websocket: %w", err)
		}
		if protocol == ProtocolWebSocket {
			return ws, nil
		}
	}
	if spdy, err = remotecommand.NewSPDYExecutor(cfg, "POST", u); err != nil {
		return nil, fmt.Errorf("exec over spdy: %w", err)
	}
	if protocol == ProtocolSPDY {
		return spdy, nil
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}

type sizeQueue <-chan TerminalSize

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q
	if !ok {
		return nil
	}
	return &remotecommand.TerminalSize{Width: s.Cols, Height: s.Rows}
}

// maxStreamStderr is how much of a stream exec's stderr Stream keeps.
const maxStreamStderr = 64 << 10

// Stream is a running exec whose stdin and stdout are one byte stream: the
// transport of a protocol that runs over a command's stdio (a pod agent
// session). Writes feed the command's stdin, reads take its stdout; Close
// ends the exec. It satisfies io.ReadWriteCloser.
type Stream struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderr  *cappedBuffer
	cancel  context.CancelFunc

	done chan struct{}
	mu   sync.Mutex
	err  error
}

// StartStream starts req (with stdin) and returns its stream at once. ctx
// is the exec's whole lifetime: cancelling it, or Close, ends the exec.
// exec runs it (Connection.Exec, or a test's stand-in).
func StartStream(ctx context.Context, req ExecRequest, exec func(context.Context, ExecRequest, Stdio) error) *Stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{stderr: &cappedBuffer{max: maxStreamStderr}, cancel: cancel, done: make(chan struct{})}
	s.stdinR, s.stdinW = io.Pipe()
	s.stdoutR, s.stdoutW = io.Pipe()
	req.Stdin, req.TTY = true, false
	go func() {
		err := exec(ctx, req, Stdio{Stdin: s.stdinR, Stdout: s.stdoutW, Stderr: lockedWriter{&s.mu, s.stderr}})
		if err == nil {
			err = io.EOF
		}
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		// Nothing reads stdin and nothing writes stdout once the exec has
		// ended: writers fail and readers drain, instead of blocking.
		_ = s.stdinR.CloseWithError(err)
		_ = s.stdoutW.CloseWithError(err)
		cancel()
		close(s.done)
	}()
	return s
}

func (s *Stream) Read(p []byte) (int, error)  { return s.stdoutR.Read(p) }
func (s *Stream) Write(p []byte) (int, error) { return s.stdinW.Write(p) }

// CloseWrite closes the command's stdin and leaves its output readable.
func (s *Stream) CloseWrite() error { return s.stdinW.Close() }

// Close ends the exec: its stdin closes and the exec is cancelled.
func (s *Stream) Close() error {
	_ = s.stdinW.Close()
	_ = s.stdoutR.Close()
	s.cancel()
	return nil
}

// Done is closed once the exec has ended.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err is how the exec ended (io.EOF for exit 0); nil while it runs.
func (s *Stream) Err() error {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	default:
		return nil
	}
}

// Stderr is the start of what the command wrote to stderr (at most 64 KiB).
func (s *Stream) Stderr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String()
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
