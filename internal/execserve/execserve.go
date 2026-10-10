// Package execserve is the daemon's side of an Exec-shaped bidi stream: it
// pipes a running command (a backend.ExecStream) over the stream's ExecIn and
// ExecOut frames until the command exits, then sends the terminal ExecExit.
// The Exec RPC (remote shells, `fleet shell`) and the local-only PodExec RPC
// (the Kubernetes kube-exec shim's daemon mode) both serve their commands
// with it, so a command behaves the same whichever stream started it.
package execserve

import (
	"context"
	"io"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ChunkSize bounds one output frame. 32 KiB matches io.Copy's default buffer;
// well under gRPC's 4 MB message ceiling.
const ChunkSize = 32 * 1024

// Stream is the server side of an Exec-shaped stream
// (grpc.BidiStreamingServer[fleetgrpc.ExecIn, fleetgrpc.ExecOut]).
type Stream interface {
	Context() context.Context
	Recv() (*fleetgrpc.ExecIn, error)
	Send(*fleetgrpc.ExecOut) error
}

// Serve pipes the running command proc over stream until it exits and sends
// the terminal exit frame; tty picks the plumbing proc was started with. It
// returns the exit code and error message the frame carried, and the send's
// error. A vanished client (stream context done) or a failed Send aborts the
// command, so commands don't outlive their callers. A client CloseSend
// half-closes stdin (without a TTY) and the command keeps running.
func Serve(stream Stream, proc backend.ExecStream, tty bool) (int32, *string, error) {
	snd := &Sender{stream: stream}
	code, errMsg := pump(stream, snd, proc, tty)
	return code, errMsg, snd.SendExit(code, errMsg)
}

func pump(stream Stream, snd *Sender, proc backend.ExecStream, tty bool) (int32, *string) {
	procDone := make(chan struct{})
	defer close(procDone)
	go closeOnDisconnect(stream.Context(), proc, procDone)

	if tty {
		pumpTTY(stream, snd, proc)
	} else {
		pumpPipes(stream, snd, proc)
	}

	code, err := proc.Wait()
	if err != nil {
		msg := err.Error()
		return int32(code), &msg
	}
	return int32(code), nil
}

// pumpTTY pipes a command on a PTY. The PTY merges stdout and stderr, so
// all output travels as stdout frames; resize frames apply to the PTY
// (including a resize that arrives before any output — the recv pump starts as
// soon as the command does).
func pumpTTY(stream Stream, snd *Sender, proc backend.ExecStream) {
	// client -> PTY. A client CloseSend (io.EOF) just stops the input feed —
	// the command keeps running until it exits on its own (or the client
	// context dies and the watcher aborts it). Interactive clients end a shell
	// with an in-band exit/^D instead of closing the stream.
	go func() {
		stdin := proc.Stdin()
		for {
			in, rerr := stream.Recv()
			if rerr != nil {
				return
			}
			switch m := in.GetMsg().(type) {
			case *fleetgrpc.ExecIn_Stdin:
				if len(m.Stdin) > 0 {
					if _, werr := stdin.Write(m.Stdin); werr != nil {
						return
					}
				}
			case *fleetgrpc.ExecIn_Resize:
				_ = proc.Resize(uint16(m.Resize.GetRows()), uint16(m.Resize.GetCols()))
			}
		}
	}()

	// PTY -> client. When the command exits the read fails (EIO from a local
	// PTY on Linux, EOF elsewhere) — either way the drain is complete. A
	// failed Send means the client is gone, so abort the command rather than
	// block forever on a full PTY buffer.
	if !pumpOutput(proc.Stdout(), snd.SendStdout) {
		_ = proc.Close()
	}
}

// pumpPipes pipes a command with separate stdin/stdout/stderr. Client EOF
// (CloseSend) closes stdin so commands that read to EOF (cat, sort, …) can
// finish; both output pumps drain fully before the caller's Wait and before
// it sends the terminal exit frame.
func pumpPipes(stream Stream, snd *Sender, proc backend.ExecStream) {
	// client -> stdin. Any recv error ends the feed; for the io.EOF of a
	// client CloseSend the deferred close is exactly the half-close the
	// command needs to see EOF. Resize frames are meaningless without a TTY
	// and are ignored (GetStdin returns nil for them).
	go func() {
		stdin := proc.Stdin()
		defer stdin.Close()
		for {
			in, rerr := stream.Recv()
			if rerr != nil {
				return
			}
			if data := in.GetStdin(); len(data) > 0 {
				if _, werr := stdin.Write(data); werr != nil {
					return
				}
			}
		}
	}()

	// stdout/stderr -> client, concurrently. A failed Send means the client
	// is gone: abort the command so a chatty process can't wedge Wait on a
	// full pipe (the context watcher usually beats us to it).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if !pumpOutput(proc.Stdout(), snd.SendStdout) {
			_ = proc.Close()
		}
	}()
	go func() {
		defer wg.Done()
		if !pumpOutput(proc.Stderr(), snd.SendStderr) {
			_ = proc.Close()
		}
	}()
	wg.Wait()
}

// pumpOutput copies r into send frames until the reader is exhausted.
// It reports false when the send side failed (client gone) rather than the
// reader simply draining.
func pumpOutput(r io.Reader, send func([]byte) error) bool {
	buf := make([]byte, ChunkSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return false
			}
		}
		if rerr != nil {
			return true
		}
	}
}

// closeOnDisconnect aborts the command when the client vanishes (stream
// context done) so commands don't outlive their callers; done releases the
// watcher once the command has been waited for.
func closeOnDisconnect(ctx context.Context, proc backend.ExecStream, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = proc.Close()
	case <-done:
	}
}

// Sender serializes Sends on an Exec-shaped stream: gRPC forbids concurrent
// Send on one ServerStream, and the non-TTY path has three writers (stdout
// pump, stderr pump, terminal exit).
type Sender struct {
	mu     sync.Mutex
	stream Stream
}

// NewSender is a Sender on stream.
func NewSender(stream Stream) *Sender { return &Sender{stream: stream} }

func (s *Sender) send(out *fleetgrpc.ExecOut) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(out)
}

// SendStdout sends a stdout frame.
func (s *Sender) SendStdout(b []byte) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stdout{Stdout: b}})
}

// SendStderr sends a stderr frame.
func (s *Sender) SendStderr(b []byte) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stderr{Stderr: b}})
}

// SendExit sends the terminal exit frame.
func (s *Sender) SendExit(code int32, errMsg *string) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Exit{Exit: &fleetgrpc.ExecExit{Code: code, Error: errMsg}}})
}
