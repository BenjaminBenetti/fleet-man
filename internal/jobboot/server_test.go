package jobboot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// socketDir is a temp dir short enough for a unix socket path.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listen(t *testing.T, opts ServerOptions) (*Server, string) {
	t.Helper()
	sock := filepath.Join(socketDir(t), SocketName)
	srv, err := Listen(sock, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, sock
}

func dial(t *testing.T, sock string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// receive is a job taking workspace transfers until one reads to a clean
// io.EOF, the way pod-job does: a failed transfer is rejected and the next one
// awaited.
func receive(ctx context.Context, srv *Server, stall time.Duration) ([]byte, []error, error) {
	var failures []error
	for {
		ws, err := srv.Workspace(ctx, stall)
		if err != nil {
			return nil, failures, err
		}
		data, err := io.ReadAll(ws)
		if err == nil {
			ws.Accept(1, int64(len(data)))
			return data, failures, nil
		}
		failures = append(failures, err)
		ws.Reject(err)
	}
}

func TestFeedDeliversTheWorkspaceAndCollectsTheResult(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "seed", Gen: 1, Workspace: true})
	tarball := randomBytes(3*feedChunk + 12345)
	ctx := context.Background()

	jobDone := make(chan error, 1)
	var got []byte
	go func() {
		data, failures, err := receive(ctx, srv, 5*time.Second)
		if err != nil || len(failures) != 0 {
			jobDone <- errors.Join(err, errors.Join(failures...))
			return
		}
		got = data
		srv.Progress(Progress{Phase: "copy", Detail: "/usr", Bytes: 42, Done: true})
		jobDone <- srv.Finish(ctx, Result{OK: true, Bytes: 99, Output: json.RawMessage(`{"v":1}`)}, 5*time.Second)
	}()

	var mu sync.Mutex
	var progress []Progress
	res, err := Feed(ctx, dial(t, sock), FeedOptions{
		Workspace: bytes.NewReader(tarball),
		Progress: func(p Progress) {
			mu.Lock()
			progress = append(progress, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-jobDone; err != nil {
		t.Fatalf("job: %v", err)
	}
	if !res.OK || res.Bytes != 99 || string(res.Output) != `{"v":1}` {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(got, tarball) {
		t.Fatalf("job got %d bytes, fleetd sent %d", len(got), len(tarball))
	}
	mu.Lock()
	defer mu.Unlock()
	phases := map[string]bool{}
	for _, p := range progress {
		phases[p.Phase] = true
	}
	if !phases["workspace"] || !phases["copy"] {
		t.Fatalf("progress %+v lacks the workspace or copy phase", progress)
	}
}

// rawClient speaks the protocol by hand, to send what Feed never would.
type rawClient struct {
	t *testing.T
	c net.Conn
}

func newRawClient(t *testing.T, sock string) *rawClient {
	rc := &rawClient{t: t, c: dial(t, sock)}
	rc.write(FrameHello, mustJSON(Hello{Proto: ProtoVersion}))
	typ, payload := rc.read()
	if typ != FrameHello {
		t.Fatalf("first frame %s %s, want the job's hello", typ, payload)
	}
	return rc
}

func (rc *rawClient) write(typ FrameType, payload []byte) {
	rc.t.Helper()
	if err := WriteFrame(rc.c, typ, payload); err != nil {
		rc.t.Fatal(err)
	}
}

func (rc *rawClient) read() (FrameType, []byte) {
	rc.t.Helper()
	_ = rc.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := ReadFrame(rc.c)
	if err != nil {
		rc.t.Fatal(err)
	}
	return typ, payload
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func TestABadChecksumIsRejectedAndTheNextTransferTaken(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	ctx := context.Background()
	type out struct {
		data     []byte
		failures []error
		err      error
	}
	jobDone := make(chan out, 1)
	go func() {
		d, f, err := receive(ctx, srv, 5*time.Second)
		jobDone <- out{d, f, err}
	}()

	rc := newRawClient(t, sock)
	rc.write(FrameWorkspace, nil)
	rc.write(FrameData, []byte("corrupted in transit"))
	rc.write(FrameEnd, mustJSON(WorkspaceEnd{Bytes: 20, SHA256: sha([]byte("what fleetd really sent"))}))
	typ, payload := rc.read()
	var msg ErrorMessage
	_ = json.Unmarshal(payload, &msg)
	if typ != FrameError || !msg.Workspace {
		t.Fatalf("after a bad checksum: %s %s, want a workspace error", typ, payload)
	}

	// The same connection retries, correctly this time.
	good := []byte("the workspace")
	rc.write(FrameWorkspace, nil)
	rc.write(FrameData, good)
	rc.write(FrameEnd, mustJSON(WorkspaceEnd{Bytes: int64(len(good)), SHA256: sha(good)}))
	o := <-jobDone
	if o.err != nil || !bytes.Equal(o.data, good) {
		t.Fatalf("job got %q, %v", o.data, o.err)
	}
	if len(o.failures) != 1 || !errors.Is(o.failures[0], ErrChecksum) {
		t.Fatalf("failures %v, want one checksum mismatch", o.failures)
	}
}

func TestFeedReportsARefusedWorkspace(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	go func() {
		ws, err := srv.Workspace(context.Background(), 5*time.Second)
		if err == nil {
			_, _ = io.ReadAll(ws)
			ws.Reject(errors.New("tar: invalid header"))
		}
	}()
	_, err := Feed(context.Background(), dial(t, sock), FeedOptions{Workspace: bytes.NewReader([]byte("x"))})
	if !errors.Is(err, ErrWorkspaceRefused) {
		t.Fatalf("Feed = %v, want ErrWorkspaceRefused", err)
	}
}

func TestADroppedTransferIsDiscardedAndANewConnectionStartsOver(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	ctx := context.Background()
	type out struct {
		data     []byte
		failures []error
		err      error
	}
	jobDone := make(chan out, 1)
	go func() {
		d, f, err := receive(ctx, srv, 5*time.Second)
		jobDone <- out{d, f, err}
	}()

	rc := newRawClient(t, sock)
	rc.write(FrameWorkspace, nil)
	rc.write(FrameData, []byte("half a tar"))
	_ = rc.c.Close()

	full := randomBytes(feedChunk + 7)
	go func() {
		res, err := Feed(ctx, dial(t, sock), FeedOptions{Workspace: bytes.NewReader(full)})
		if err == nil && !res.OK {
			t.Errorf("unexpected result %+v", res)
		}
	}()
	o := <-jobDone
	if o.err != nil || !bytes.Equal(o.data, full) {
		t.Fatalf("job got %d bytes, %v", len(o.data), o.err)
	}
	if len(o.failures) != 1 || !errors.Is(o.failures[0], ErrDropped) {
		t.Fatalf("failures %v, want one dropped transfer", o.failures)
	}
	_ = srv.Finish(ctx, Result{OK: true}, time.Second)
}

func TestANewerConnectionReplacesTheCurrentOne(t *testing.T) {
	_, sock := listen(t, ServerOptions{Workspace: true})
	first := newRawClient(t, sock)
	newRawClient(t, sock)
	_ = first.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ReadFrame(first.c); err == nil {
		t.Fatal("the replaced connection is still open")
	}
}

func TestStallTimeouts(t *testing.T) {
	t.Run("nobody connects", func(t *testing.T) {
		srv, _ := listen(t, ServerOptions{Workspace: true})
		start := time.Now()
		_, err := srv.Workspace(context.Background(), 300*time.Millisecond)
		if !errors.Is(err, ErrStalled) {
			t.Fatalf("Workspace = %v, want ErrStalled", err)
		}
		if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
			t.Fatalf("stalled after %s", d)
		}
	})
	t.Run("the transfer stops", func(t *testing.T) {
		srv, sock := listen(t, ServerOptions{Workspace: true})
		rc := newRawClient(t, sock)
		rc.write(FrameWorkspace, nil)
		rc.write(FrameData, []byte("some"))
		ws, err := srv.Workspace(context.Background(), 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		// Pings keep the transport alive but are not bytes of the workspace.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(50 * time.Millisecond):
					_ = WriteFrame(rc.c, FramePing, nil)
				}
			}
		}()
		data, err := io.ReadAll(ws)
		if !errors.Is(err, ErrStalled) || string(data) != "some" {
			t.Fatalf("read %q, %v; want the data then ErrStalled", data, err)
		}
	})
	t.Run("ctx ends", func(t *testing.T) {
		srv, _ := listen(t, ServerOptions{Workspace: true})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := srv.Workspace(ctx, time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("Workspace = %v", err)
		}
	})
}

func TestALaterConnectionCollectsAFinishedResult(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "rollback", Gen: 3})
	finished := make(chan error, 1)
	go func() {
		finished <- srv.Finish(context.Background(), Result{OK: false, Error: "boom"}, 10*time.Second)
	}()
	time.Sleep(100 * time.Millisecond)
	res, err := Feed(context.Background(), dial(t, sock), FeedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error != "boom" {
		t.Fatalf("result %+v", res)
	}
	if err := <-finished; err != nil {
		t.Fatalf("Finish = %v after fleetd acknowledged", err)
	}
}

func TestAnUncollectedResultTimesOut(t *testing.T) {
	srv, _ := listen(t, ServerOptions{})
	if err := srv.Finish(context.Background(), Result{OK: true}, 100*time.Millisecond); !errors.Is(err, ErrNotCollected) {
		t.Fatalf("Finish = %v, want ErrNotCollected", err)
	}
}

func TestFeedNeedsAWorkspaceTheJobWants(t *testing.T) {
	_, sock := listen(t, ServerOptions{Workspace: true})
	if _, err := Feed(context.Background(), dial(t, sock), FeedOptions{}); err == nil {
		t.Fatal("Feed without a workspace for a job that wants one succeeded")
	}
}

func TestAWorkspaceTheJobDoesNotWantIsRefused(t *testing.T) {
	_, sock := listen(t, ServerOptions{Workspace: false})
	rc := newRawClient(t, sock)
	rc.write(FrameWorkspace, nil)
	rc.write(FrameData, []byte("unwanted"))
	rc.write(FrameEnd, mustJSON(WorkspaceEnd{Bytes: 8, SHA256: sha([]byte("unwanted"))}))
	typ, payload := rc.read()
	if typ != FrameError {
		t.Fatalf("got %s %s, want an error", typ, payload)
	}
	// The connection survives the refusal.
	rc.write(FramePing, nil)
}

func TestAProtocolMismatchIsRefused(t *testing.T) {
	_, sock := listen(t, ServerOptions{})
	c := dial(t, sock)
	_ = WriteJSON(c, FrameHello, Hello{Proto: ProtoVersion + 1})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := ReadFrame(c)
	if err != nil || typ != FrameError {
		t.Fatalf("got %s %s %v, want an error", typ, payload, err)
	}
}

func TestRunPipeCarriesFeedOverExecStdio(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	ctx := context.Background()
	tarball := randomBytes(2*feedChunk + 1)
	jobDone := make(chan []byte, 1)
	go func() {
		data, _, err := receive(ctx, srv, 5*time.Second)
		if err != nil {
			jobDone <- nil
			return
		}
		jobDone <- data
		_ = srv.Finish(ctx, Result{OK: true, Output: json.RawMessage(`{"piped":true}`)}, 5*time.Second)
		_ = srv.Close()
	}()

	// The exec's stdio: fleetd writes the pipe's stdin and reads its stdout.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	pipeDone := make(chan error, 1)
	go func() {
		err := RunPipe(ctx, sock, stdinR, stdoutW, 5*time.Second)
		_ = stdoutW.Close()
		pipeDone <- err
	}()
	res, err := Feed(ctx, struct {
		io.Reader
		io.Writer
	}{stdoutR, stdinW}, FeedOptions{Workspace: bytes.NewReader(tarball)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || string(res.Output) != `{"piped":true}` {
		t.Fatalf("result %+v", res)
	}
	if got := <-jobDone; !bytes.Equal(got, tarball) {
		t.Fatalf("job got %d bytes, want %d", len(got), len(tarball))
	}
	_ = stdinW.Close()
	select {
	case err := <-pipeDone:
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pipe did not exit once the job closed its end")
	}
}

func TestRunPipeGivesUpWithoutASocket(t *testing.T) {
	err := RunPipe(context.Background(), filepath.Join(socketDir(t), "none.sock"), bytes.NewReader(nil), io.Discard, 300*time.Millisecond)
	if err == nil {
		t.Fatal("RunPipe to no socket succeeded")
	}
}

// sendTransfer writes one whole, well-formed transfer of data in frames of at
// most chunk bytes. It reports the first write error rather than failing the
// test, so it may run in a goroutine of its own.
func (rc *rawClient) sendTransfer(data []byte, chunk int) error {
	if err := WriteFrame(rc.c, FrameWorkspace, nil); err != nil {
		return err
	}
	for off := 0; off < len(data); off += chunk {
		if err := WriteFrame(rc.c, FrameData, data[off:min(off+chunk, len(data))]); err != nil {
			return err
		}
	}
	return WriteJSON(rc.c, FrameEnd, WorkspaceEnd{Bytes: int64(len(data)), SHA256: sha(data)})
}

func TestAcceptAbandonsATransferANewerConnectionStarted(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	ctx := context.Background()
	first := newRawClient(t, sock)
	if err := first.sendTransfer([]byte("the workspace"), 4); err != nil {
		t.Fatal(err)
	}
	ws, err := srv.Workspace(ctx, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(ws); err != nil || string(data) != "the workspace" {
		t.Fatalf("read %q, %v", data, err)
	}

	// Before the job accepts it, fleetd reconnects and sends the workspace
	// again: more frames than the transfer buffers, so the newer
	// connection's read loop waits on a transfer nobody will read.
	second := newRawClient(t, sock)
	go func() { _ = second.sendTransfer(bytes.Repeat([]byte("x"), 64), 2) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.attempts) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second transfer never reached the job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ws.Accept(1, 13)

	// The result goes to the newer connection, and its acknowledgement must
	// get through.
	finished := make(chan error, 1)
	go func() { finished <- srv.Finish(ctx, Result{OK: true}, 3*time.Second) }()
	for {
		typ, _ := second.read()
		if typ == FrameResult {
			break
		}
	}
	second.write(FrameAck, nil)
	if err := <-finished; err != nil {
		t.Fatalf("Finish = %v, want the acknowledged result", err)
	}
}

func TestFinishHoldsNoLongerThanTheHoldOnAStuckConnection(t *testing.T) {
	srv, sock := listen(t, ServerOptions{})
	newRawClient(t, sock) // and never reads again
	// Progress fills the socket and then the connection's queue.
	big := Progress{Phase: "copy", Detail: string(bytes.Repeat([]byte("p"), 64<<10))}
	for range 200 {
		srv.Progress(big)
	}
	finished := make(chan error, 1)
	go func() { finished <- srv.Finish(context.Background(), Result{OK: true}, 300*time.Millisecond) }()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrNotCollected) {
			t.Fatalf("Finish = %v, want ErrNotCollected", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Finish blocked past its hold on a connection that does not read")
	}
}

func TestAStallNeverHidesWhatAlreadyArrived(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Workspace: true})
	data := randomBytes(3000)
	if err := newRawClient(t, sock).sendTransfer(data, 1000); err != nil {
		t.Fatal(err)
	}
	ws, err := srv.Workspace(context.Background(), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// The job is slow (its disk, say) and fleetd has nothing more to send.
	time.Sleep(500 * time.Millisecond)
	got, err := io.ReadAll(ws)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, %v; want the whole verified transfer", len(got), err)
	}
}
