package jobboot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// feedChunk is how much workspace a FrameData carries.
const feedChunk = 256 << 10

// ErrWorkspaceRefused is a workspace transfer the job refused (a checksum
// mismatch, a tar it could not extract, or one it did not want). The job
// discarded it; a fresh Feed can send it again.
var ErrWorkspaceRefused = errors.New("the job refused the workspace")

// FeedOptions configure Feed.
type FeedOptions struct {
	// Workspace is the workspace tar to send if the job wants one; nil sends
	// none (Feed then fails if the job still wants one).
	Workspace io.Reader
	// Progress receives the job's progress frames; nil drops them.
	Progress func(Progress)
	// Barrier does what the job waits for at a barrier (a clone's copier
	// before and after its final delta); its error refuses the barrier,
	// which fails the job. It runs beside the feed, which tells the job
	// every BarrierKeepalive that fleetd is still at it, and Feed waits for
	// it before returning (its ctx ends with Feed's). A barrier sent again
	// on a newer connection runs again, so it must be idempotent. nil refuses
	// every barrier.
	Barrier func(ctx context.Context, b Barrier) error
}

// BarrierKeepalive is how often Feed tells a job waiting at a barrier that
// fleetd is still working on it.
var BarrierKeepalive = 30 * time.Second

// Feed is fleetd's side of the job socket, over conn (a kube exec of `fleet
// pod-job feed`, or a bulk-session stream dialed to the socket): the hello,
// the workspace when the job wants it, progress until the job's Result, and
// the acknowledgement that lets the job exit. A job that failed is a Result
// with OK false and a nil error; the error is for the transport, the protocol
// and ErrWorkspaceRefused. Feed does not close conn; when ctx ends it returns
// ctx's error and the caller closes conn, which ends Feed's reader.
func Feed(ctx context.Context, conn io.ReadWriter, opts FeedOptions) (Result, error) {
	var wmu sync.Mutex
	write := func(t FrameType, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return WriteFrame(conn, t, payload)
	}
	hello, _ := json.Marshal(Hello{Proto: ProtoVersion})
	if err := write(FrameHello, hello); err != nil {
		return Result{}, fmt.Errorf("jobboot hello: %w", err)
	}

	// Barriers run beside the read loop, and end (their ctx cancelled) before
	// Feed returns: what one does must never outlive the feed that started
	// it (a freeze racing the caller's clean-up).
	// The reader may still read a barrier once Feed has begun to return
	// (its ctx ended first): that one is refused, never run.
	bctx, bcancel := context.WithCancel(ctx)
	var (
		barriers sync.WaitGroup
		bmu      sync.Mutex
		bclosed  bool
	)
	defer func() {
		bmu.Lock()
		bclosed = true
		bmu.Unlock()
		bcancel()
		barriers.Wait()
	}()
	runBarrier := func(b Barrier) {
		defer barriers.Done()
		release := func(r Release) {
			data, _ := json.Marshal(r)
			_ = write(FrameRelease, data)
		}
		if opts.Barrier == nil {
			release(Release{Seq: b.Seq, Error: fmt.Sprintf("fleetd has nothing to do at barrier %q", b.Name)})
			return
		}
		finished := make(chan error, 1)
		go func() { finished <- opts.Barrier(bctx, b) }()
		tick := time.NewTicker(BarrierKeepalive)
		defer tick.Stop()
		for {
			select {
			case err := <-finished:
				if err != nil {
					release(Release{Seq: b.Seq, Error: err.Error()})
				} else {
					release(Release{Seq: b.Seq, OK: true})
				}
				return
			case <-tick.C:
				release(Release{Seq: b.Seq, Wait: true})
			}
		}
	}

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	sendErr := make(chan error, 1)
	go func() {
		done <- func() outcome {
			sawHello := false
			for {
				t, payload, err := ReadFrame(conn)
				if err != nil {
					if errors.Is(err, io.EOF) {
						err = io.ErrUnexpectedEOF
					}
					return outcome{err: fmt.Errorf("jobboot: %w", err)}
				}
				switch t {
				case FrameHello:
					var h Hello
					if err := json.Unmarshal(payload, &h); err != nil {
						return outcome{err: fmt.Errorf("jobboot: bad hello: %w", err)}
					}
					if h.Proto != ProtoVersion {
						return outcome{err: fmt.Errorf("jobboot: the job speaks protocol %d, this fleet %d", h.Proto, ProtoVersion)}
					}
					sawHello = true
					if h.Done || !h.Workspace {
						continue
					}
					if opts.Workspace == nil {
						return outcome{err: errors.New("jobboot: the job wants a workspace and none was given")}
					}
					go func() { sendErr <- sendWorkspace(opts.Workspace, write) }()
				case FrameProgress:
					var p Progress
					if json.Unmarshal(payload, &p) == nil && opts.Progress != nil {
						opts.Progress(p)
					}
				case FrameError:
					var msg ErrorMessage
					_ = json.Unmarshal(payload, &msg)
					if msg.Workspace {
						return outcome{err: fmt.Errorf("%w: %s", ErrWorkspaceRefused, msg.Error)}
					}
					return outcome{err: fmt.Errorf("jobboot: the job reports: %s", msg.Error)}
				case FrameResult:
					if !sawHello {
						return outcome{err: errors.New("jobboot: result before the hello")}
					}
					var r Result
					if err := json.Unmarshal(payload, &r); err != nil {
						return outcome{err: fmt.Errorf("jobboot: bad result: %w", err)}
					}
					if err := write(FrameAck, nil); err != nil {
						return outcome{result: r, err: fmt.Errorf("jobboot: acknowledge the result: %w", err)}
					}
					return outcome{result: r}
				case FrameBarrier:
					var b Barrier
					if err := json.Unmarshal(payload, &b); err != nil {
						return outcome{err: fmt.Errorf("jobboot: bad barrier: %w", err)}
					}
					bmu.Lock()
					if bclosed {
						bmu.Unlock()
						return outcome{err: errors.New("jobboot: a barrier after the feed ended")}
					}
					barriers.Add(1)
					bmu.Unlock()
					go runBarrier(b)
				case FramePing:
				default:
					return outcome{err: fmt.Errorf("jobboot: unexpected %s frame from the job", t)}
				}
			}
		}()
	}()

	for {
		select {
		case o := <-done:
			return o.result, o.err
		case err := <-sendErr:
			if err != nil {
				return Result{}, err
			}
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
}

// sendWorkspace sends ws as one transfer: its data, then its length and sha.
func sendWorkspace(ws io.Reader, write func(FrameType, []byte) error) error {
	if err := write(FrameWorkspace, nil); err != nil {
		return fmt.Errorf("jobboot: start the workspace: %w", err)
	}
	h := sha256.New()
	var n int64
	buf := make([]byte, feedChunk)
	for {
		k, err := ws.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			if werr := write(FrameData, buf[:k]); werr != nil {
				return fmt.Errorf("jobboot: send the workspace: %w", werr)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("jobboot: read the workspace: %w", err)
		}
	}
	end, _ := json.Marshal(WorkspaceEnd{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	if err := write(FrameEnd, end); err != nil {
		return fmt.Errorf("jobboot: end the workspace: %w", err)
	}
	return nil
}
