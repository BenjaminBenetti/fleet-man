// Package execstream is the client half of the Exec bidi RPC: it runs a
// command inside an instance's container by streaming stdin/stdout/stderr
// (and TTY resizes) between the local process and the fleet daemon. It exists
// for the REMOTE daemon case (FLEET_GATEWAY / FLEET_SERVER): the TTY
// carve-out — ResolveExecCommand + exec the returned argv locally — cannot
// work there, because that argv references binaries and containers that only
// exist on the daemon's host.
//
// BOUNDARY: like internal/fleetclient, this package must never import the
// server-only internals (internal/state, internal/backend, internal/server,
// internal/flog, ...) — see the depguard note in .golangci.yml. It depends
// only on fleetgrpc, grpc, and terminal helpers.
package execstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/charmbracelet/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options configures one streamed exec.
type Options struct {
	Fleet    string
	Instance string
	// Argv is the command to run inside the instance.
	Argv []string
	// TTY requests a PTY inside the container. When Stdin is additionally the
	// local terminal, it is put into raw mode (always restored on return) and
	// window resizes are forwarded to the server.
	TTY bool
	// Env is extra environment for the in-container command (may be nil).
	Env map[string]string
	// Stdin is pumped to the remote command until EOF; nil half-closes the
	// send side immediately (the server treats client EOF as stdin EOF).
	Stdin io.Reader
	// Stdout and Stderr receive the remote command's output frames. In TTY
	// mode the server merges stderr into stdout. A nil writer discards.
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes opts over svc's Exec stream and blocks until the remote command
// exits, returning its exit code. The exit code is meaningful when err is nil,
// or when err carries the ExecExit error string (the command ran but the
// server reported a failure alongside the code); transport errors return -1.
func Run(ctx context.Context, svc fleetgrpc.FleetServiceClient, opts Options) (int, error) {
	// Cancelling on return tears down the stream, which unblocks the stdin
	// pump's pending Send. (The pump's Read on a real terminal may stay
	// blocked until process exit — harmless for the CLI lifetimes this serves.)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := svc.Exec(ctx)
	if err != nil {
		return -1, mapRPCError(err)
	}
	return run(st, opts)
}

// Output runs argv non-interactively (no TTY, no stdin) and returns the
// collected stdout — the one-shot convenience. Stderr is discarded, matching
// the `2>/dev/null` style of the call sites it serves.
func Output(ctx context.Context, svc fleetgrpc.FleetServiceClient, fleet, instance string, argv []string) (stdout []byte, exitCode int, err error) {
	var buf bytes.Buffer
	code, err := Run(ctx, svc, Options{
		Fleet:    fleet,
		Instance: instance,
		Argv:     argv,
		Stdout:   &buf,
	})
	return buf.Bytes(), code, err
}

// stream is the subset of the generated Exec client stream run needs; tests
// hand-roll one.
type stream interface {
	Send(*fleetgrpc.ExecIn) error
	Recv() (*fleetgrpc.ExecOut, error)
	CloseSend() error
}

// run drives an already-open stream: start frame, terminal setup, stdin pump,
// then the receive loop until the terminal ExecExit.
func run(st stream, opts Options) (int, error) {
	// gRPC permits only one concurrent sender per stream; the stdin pump and
	// the SIGWINCH handler both send, so serialize (CloseSend included).
	var sendMu sync.Mutex
	send := func(in *fleetgrpc.ExecIn) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return st.Send(in)
	}
	closeSend := func() error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return st.CloseSend()
	}

	if err := send(&fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Start{Start: &fleetgrpc.ExecStart{
		Fleet:    opts.Fleet,
		Instance: opts.Instance,
		Argv:     opts.Argv,
		Tty:      opts.TTY,
		Env:      opts.Env,
	}}}); err != nil {
		// gRPC's Send reports a bare io.EOF on a dead stream; the real status
		// comes from Recv.
		if _, rerr := st.Recv(); rerr != nil && !errors.Is(rerr, io.EOF) {
			return -1, mapRPCError(rerr)
		}
		return -1, mapRPCError(err)
	}

	if opts.TTY {
		if cleanup := setupTerminal(send, opts.Stdin); cleanup != nil {
			defer cleanup()
		}
	}

	go pumpStdin(send, closeSend, opts.Stdin)

	for {
		out, err := st.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return -1, errors.New("exec stream closed without an exit status")
			}
			return -1, mapRPCError(err)
		}
		switch msg := out.GetMsg().(type) {
		case *fleetgrpc.ExecOut_Stdout:
			if opts.Stdout != nil {
				if _, err := opts.Stdout.Write(msg.Stdout); err != nil {
					return -1, err
				}
			}
		case *fleetgrpc.ExecOut_Stderr:
			if opts.Stderr != nil {
				if _, err := opts.Stderr.Write(msg.Stderr); err != nil {
					return -1, err
				}
			}
		case *fleetgrpc.ExecOut_Exit:
			code := int(msg.Exit.GetCode())
			if errMsg := msg.Exit.GetError(); errMsg != "" {
				return code, errors.New(errMsg)
			}
			return code, nil
		}
	}
}

// pumpStdin copies stdin to the stream until EOF, then half-closes the send
// side (the server treats client EOF as stdin EOF). A nil stdin half-closes
// immediately — the one-shot case. A send failure just stops the pump: the
// receive loop reports the stream's real error.
func pumpStdin(send func(*fleetgrpc.ExecIn) error, closeSend func() error, stdin io.Reader) {
	if stdin == nil {
		_ = closeSend()
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := stdin.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if send(&fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Stdin{Stdin: data}}) != nil {
				return
			}
		}
		if rerr != nil { // io.EOF or a real read error — either way stdin is done
			_ = closeSend()
			return
		}
	}
}

// setupTerminal puts a real-terminal stdin into raw mode, sends the initial
// window size, and forwards SIGWINCH resizes. It returns a cleanup func that
// ALWAYS restores the terminal, or nil when stdin is not a terminal (a pipe,
// a test reader) — then there is nothing local to mirror; the server still
// allocates the PTY.
func setupTerminal(send func(*fleetgrpc.ExecIn) error, stdin io.Reader) (cleanup func()) {
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return nil
	}

	state, rawErr := term.MakeRaw(f.Fd())

	sendResize(send, f)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			sendResize(send, f)
		}
	}()

	return func() {
		signal.Stop(winch) // guarantees no further sends, so the close is safe
		close(winch)
		if rawErr == nil {
			_ = term.Restore(f.Fd(), state)
		}
	}
}

// sendResize sends the terminal's current size; a failed size read is simply
// skipped (the server keeps the previous size).
func sendResize(send func(*fleetgrpc.ExecIn) error, f *os.File) {
	cols, rows, err := term.GetSize(f.Fd())
	if err != nil || cols <= 0 || rows <= 0 {
		return
	}
	_ = send(&fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Resize{Resize: &fleetgrpc.ExecResize{
		Rows: uint32(rows),
		Cols: uint32(cols),
	}}})
}

// mapRPCError rewrites the version-skew case — an older daemon without the
// Exec handler — into an actionable message; other errors pass through.
func mapRPCError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("remote daemon does not support streaming exec — upgrade fleetd")
	}
	return err
}
