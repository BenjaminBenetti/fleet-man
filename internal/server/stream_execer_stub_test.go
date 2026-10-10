package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// stream_execer_stub_test.go: an in-memory backend.ExecStream and a backend
// that hands them out, for the paths that prefer a StreamExecer (the Exec RPC,
// the mic sink, the audio output source).

// fakeExecStream is a command running "inside the instance": the test plays
// the command through the far ends of its pipes (cmdStdin, cmdStdout,
// cmdStderr) and ends it with exit.
type fakeExecStream struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	stderr *io.PipeReader

	cmdStdin  *io.PipeReader
	cmdStdout *io.PipeWriter
	cmdStderr *io.PipeWriter

	mu      sync.Mutex
	resizes [][2]uint16
	closes  int
	waits   int

	exited   chan struct{}
	exitOnce sync.Once
	code     int
	err      error
}

func newFakeExecStream() *fakeExecStream {
	s := &fakeExecStream{exited: make(chan struct{})}
	s.cmdStdin, s.stdin = io.Pipe()
	s.stdout, s.cmdStdout = io.Pipe()
	s.stderr, s.cmdStderr = io.Pipe()
	return s
}

// exit ends the command with code (or a broken stream, err): its output ends
// and Wait returns.
func (s *fakeExecStream) exit(code int, err error) {
	s.exitOnce.Do(func() {
		s.code, s.err = code, err
		_ = s.cmdStdout.Close()
		_ = s.cmdStderr.Close()
		_ = s.cmdStdin.Close()
		close(s.exited)
	})
}

func (s *fakeExecStream) Stdin() io.WriteCloser { return s.stdin }
func (s *fakeExecStream) Stdout() io.Reader     { return s.stdout }
func (s *fakeExecStream) Stderr() io.Reader     { return s.stderr }

func (s *fakeExecStream) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizes = append(s.resizes, [2]uint16{rows, cols})
	return nil
}

func (s *fakeExecStream) Wait() (int, error) {
	<-s.exited
	s.mu.Lock()
	s.waits++
	s.mu.Unlock()
	return s.code, s.err
}

// Close aborts the command as a signal would: exit code -1.
func (s *fakeExecStream) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	s.exit(-1, nil)
	return nil
}

func (s *fakeExecStream) counts() (closes, waits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes, s.waits
}

// waitFor polls cond until it holds or the test's patience runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamExecer is a backend whose commands run as fakeExecStreams. Any Backend
// method it does not define panics on the nil embedded interface: the paths
// under test must not fall back to host commands.
type streamExecer struct {
	backend.Backend
	micSink bool

	mu      sync.Mutex
	reqs    []backend.ExecRequest
	streams []*fakeExecStream
	openErr error
}

func (b *streamExecer) OpenExec(ctx context.Context, req backend.ExecRequest) (backend.ExecStream, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("OpenExec needs a live ctx")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, req)
	if b.openErr != nil {
		return nil, b.openErr
	}
	s := newFakeExecStream()
	b.streams = append(b.streams, s)
	return s, nil
}

func (b *streamExecer) SupportsMicSink() bool { return b.micSink }

func (b *streamExecer) opened() ([]backend.ExecRequest, []*fakeExecStream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.ExecRequest(nil), b.reqs...), append([]*fakeExecStream(nil), b.streams...)
}
