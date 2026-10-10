//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterBoundsInFlight(t *testing.T) {
	l := newLimiter(4)
	var inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.acquire(context.Background(), PriorityBackground)
			if err != nil {
				t.Error(err)
				return
			}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			release()
			release() // a second release is a no-op
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 4 {
		t.Fatalf("peak in flight %d, want 4", p)
	}
	if n := l.inFlight(); n != 0 {
		t.Fatalf("%d slots still taken", n)
	}
}

func TestLimiterGrantsByPriorityThenArrival(t *testing.T) {
	l := newLimiter(1)
	hold, _ := l.acquire(context.Background(), PriorityBackground)
	order := make(chan string, 4)
	queued := 0
	start := func(name string, prio Priority) {
		go func() {
			release, err := l.acquire(context.Background(), prio)
			if err != nil {
				t.Error(err)
				return
			}
			order <- name
			release()
		}()
		queued++
		waitQueued(t, l, queued)
	}
	start("background", PriorityBackground)
	start("state", PriorityState)
	start("interactive-1", PriorityInteractive)
	start("interactive-2", PriorityInteractive)
	hold()
	var got []string
	for range 4 {
		got = append(got, <-order)
	}
	want := []string{"interactive-1", "interactive-2", "state", "background"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("granted %v, want %v", got, want)
		}
	}
}

// waitQueued waits until n acquires are queued on l.
func waitQueued(t *testing.T, l *limiter, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		got := l.queue.Len()
		l.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d queued, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLimiterWaitEndsWithTheContext(t *testing.T) {
	l := newLimiter(1)
	hold, _ := l.acquire(context.Background(), PriorityBackground)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, PriorityInteractive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	hold()
	// The abandoned wait left no ghost in the queue: the slot is free.
	release, err := l.acquire(context.Background(), PriorityBackground)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if n := l.inFlight(); n != 0 {
		t.Fatalf("%d slots taken", n)
	}
}

func TestFullJitter(t *testing.T) {
	base, limit := time.Second, 30*time.Second
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 40: 30 * time.Second} {
		if got := fullJitter(n, base, limit, 0.999999999); got > want || got < want*99/100 {
			t.Errorf("attempt %d: max wait %v, want just under %v", n, got, want)
		}
		if got := fullJitter(n, base, limit, 0); got != 0 {
			t.Errorf("attempt %d: min wait %v, want 0", n, got)
		}
	}
	if got := fullJitter(3, base, limit, 0.5); got != 2*time.Second {
		t.Errorf("half of attempt 3's window: %v", got)
	}
}

func TestTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTokenBucket(2, 3, func() time.Time { return now })
	for range 3 {
		if err := b.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fourth token in the same instant: %v", err)
	}
	now = now.Add(500 * time.Millisecond) // one token at 2/s
	if err := b.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour) // refills to the burst, not past it
	for range 3 {
		if err := b.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := b.wait(ctx2); err == nil {
		t.Fatal("the bucket held more than its burst")
	}
}

// A queued ticket raised by a more urgent caller moves ahead of waiters it
// now outranks, keeping its arrival order among equals.
func TestLimiterRaisesAQueuedTicket(t *testing.T) {
	l := newLimiter(1)
	hold, _ := l.acquire(context.Background(), PriorityBackground)
	order := make(chan string, 3)
	queued := 0
	start := func(name string, tk *ticket) {
		go func() {
			release, err := l.acquireTicket(context.Background(), tk)
			if err != nil {
				t.Error(err)
				return
			}
			order <- name
			release()
		}()
		queued++
		waitQueued(t, l, queued)
	}
	poke := newTicket(PriorityBackground)
	start("poke", poke)
	start("state", newTicket(PriorityState))
	start("interactive", newTicket(PriorityInteractive))
	// A pane opens on the poked pod: its bootstrap is now interactive, and
	// it was queued before the other interactive one.
	poke.raise(PriorityInteractive)
	poke.raise(PriorityBackground) // never lowered
	hold()
	var got []string
	for range 3 {
		got = append(got, <-order)
	}
	want := []string{"poke", "interactive", "state"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("granted %v, want %v", got, want)
		}
	}
	// Raising a granted ticket is a no-op.
	poke.raise(PriorityInteractive + 1)
	// The last holder releases just after it reported its turn.
	deadline := time.Now().Add(5 * time.Second)
	for l.inFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots taken", l.inFlight())
		}
		time.Sleep(time.Millisecond)
	}
}
