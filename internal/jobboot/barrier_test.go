package jobboot

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// barrierJob is a job that waits at the named barriers in turn, then
// finishes with OK and the releases it got as its error text.
func barrierJob(ctx context.Context, srv *Server, stall time.Duration, names ...string) (<-chan []Release, <-chan error) {
	rels := make(chan []Release, 1)
	errc := make(chan error, 1)
	go func() {
		var got []Release
		for _, n := range names {
			r, err := srv.Barrier(ctx, n, "detail of "+n, stall)
			if err != nil {
				rels <- got
				errc <- err
				return
			}
			got = append(got, r)
		}
		rels <- got
		errc <- srv.Finish(ctx, Result{OK: true}, 5*time.Second)
	}()
	return rels, errc
}

func TestBarriersRunInFleetdInTurn(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 2})
	ctx := context.Background()
	rels, jobErr := barrierJob(ctx, srv, 5*time.Second, "final", "copied")

	var seen []Barrier
	res, err := Feed(ctx, dial(t, sock), FeedOptions{Barrier: func(_ context.Context, b Barrier) error {
		seen = append(seen, b)
		return nil
	}})
	if err != nil || !res.OK {
		t.Fatalf("feed: %+v, %v", res, err)
	}
	if err := <-jobErr; err != nil {
		t.Fatalf("job: %v", err)
	}
	got := <-rels
	if len(seen) != 2 || seen[0].Name != "final" || seen[0].Seq != 1 || seen[1].Name != "copied" || seen[1].Seq != 2 || seen[1].Detail != "detail of copied" {
		t.Fatalf("fleetd saw barriers %+v", seen)
	}
	if len(got) != 2 || !got[0].OK || got[0].Seq != 1 || !got[1].OK || got[1].Seq != 2 {
		t.Fatalf("the job got releases %+v", got)
	}
}

func TestARefusedBarrierReachesTheJob(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 1})
	ctx := context.Background()
	rels, jobErr := barrierJob(ctx, srv, 5*time.Second, "final")
	res, err := Feed(ctx, dial(t, sock), FeedOptions{Barrier: func(context.Context, Barrier) error {
		return errors.New("the source could not be frozen")
	}})
	if err != nil || !res.OK {
		t.Fatalf("feed: %+v, %v", res, err)
	}
	if err := <-jobErr; err != nil {
		t.Fatal(err)
	}
	got := <-rels
	if len(got) != 1 || got[0].OK || got[0].Error != "the source could not be frozen" {
		t.Fatalf("release %+v", got)
	}
}

func TestFeedWithoutABarrierHandlerRefusesIt(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 1})
	ctx := context.Background()
	rels, jobErr := barrierJob(ctx, srv, 5*time.Second, "final")
	if _, err := Feed(ctx, dial(t, sock), FeedOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-jobErr; err != nil {
		t.Fatal(err)
	}
	if got := <-rels; len(got) != 1 || got[0].OK || !strings.Contains(got[0].Error, `barrier "final"`) {
		t.Fatalf("release %+v", got)
	}
}

func TestASlowBarrierKeepsTheJobFromStalling(t *testing.T) {
	old := BarrierKeepalive
	BarrierKeepalive = 20 * time.Millisecond
	t.Cleanup(func() { BarrierKeepalive = old })
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 1})
	ctx := context.Background()
	// The job stalls after 150ms of silence; fleetd works for 600ms, saying
	// so every 20ms.
	rels, jobErr := barrierJob(ctx, srv, 150*time.Millisecond, "final")
	if _, err := Feed(ctx, dial(t, sock), FeedOptions{Barrier: func(context.Context, Barrier) error {
		time.Sleep(600 * time.Millisecond)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-jobErr; err != nil {
		t.Fatalf("the job stalled at the barrier: %v", err)
	}
	if got := <-rels; len(got) != 1 || !got[0].OK {
		t.Fatalf("release %+v", got)
	}
}

func TestABarrierStallsWithoutFleetd(t *testing.T) {
	srv, _ := listen(t, ServerOptions{Op: "clone", Gen: 1})
	// The job worked a while before the barrier: fleetd's silence is counted
	// from the barrier on, not from its last byte.
	time.Sleep(80 * time.Millisecond)
	start := time.Now()
	_, err := srv.Barrier(context.Background(), "final", "", 60*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("barrier with nobody there: %v, want ErrStalled", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("stalled after %s: the silence before the barrier was counted", d)
	}
}

func TestABarrierIsSentToTheConnectionThatComesLater(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 1})
	ctx := context.Background()
	rels, jobErr := barrierJob(ctx, srv, 5*time.Second, "final")
	time.Sleep(50 * time.Millisecond) // waiting at the barrier, nobody connected
	var calls atomic.Int32
	if _, err := Feed(ctx, dial(t, sock), FeedOptions{Barrier: func(context.Context, Barrier) error {
		calls.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-jobErr; err != nil {
		t.Fatal(err)
	}
	if got := <-rels; len(got) != 1 || !got[0].OK || calls.Load() != 1 {
		t.Fatalf("release %+v after %d calls", got, calls.Load())
	}
}

func TestFeedEndsItsBarriersBeforeReturning(t *testing.T) {
	srv, sock := listen(t, ServerOptions{Op: "clone", Gen: 1})
	jobCtx, cancelJob := context.WithCancel(context.Background())
	defer cancelJob()
	_, _ = barrierJob(jobCtx, srv, 5*time.Second, "final")

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var ended atomic.Bool
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		_, _ = Feed(ctx, dial(t, sock), FeedOptions{Barrier: func(bctx context.Context, _ Barrier) error {
			close(started)
			<-bctx.Done()
			time.Sleep(30 * time.Millisecond)
			ended.Store(true)
			return bctx.Err()
		}})
	}()
	<-started
	cancel()
	<-feedDone
	if !ended.Load() {
		t.Fatal("Feed returned while its barrier still ran")
	}
}

// A barrier the reader reads once Feed has begun to return (its ctx ended)
// is never run: what it would do (freeze or stop a source) would outlive
// the feed, and the caller's clean-up.
func TestABarrierAfterTheFeedEndedNeverRuns(t *testing.T) {
	fleetd, job := net.Pipe()
	t.Cleanup(func() { _ = fleetd.Close(); _ = job.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Feed(ctx, fleetd, FeedOptions{Barrier: func(context.Context, Barrier) error {
			calls.Add(1)
			return nil
		}})
	}()
	if _, _, err := ReadFrame(job); err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(Hello{Proto: ProtoVersion})
	if err := WriteFrame(job, FrameHello, hello); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	b, _ := json.Marshal(Barrier{Seq: 1, Name: "final"})
	if err := WriteFrame(job, FrameBarrier, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("a barrier read after the feed ended ran %d times", n)
	}
}

func TestBarrierFramesRoundTrip(t *testing.T) {
	for _, ft := range []FrameType{FrameBarrier, FrameRelease} {
		if !ft.valid() {
			t.Errorf("%s is not a valid frame type", ft)
		}
	}
}
