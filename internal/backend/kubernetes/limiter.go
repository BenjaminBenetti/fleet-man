//go:build !nok8s

package kubernetes

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Priority orders the bootstraps waiting on a connection's limiter.
type Priority int

const (
	// PriorityBackground: nothing is waiting on the pod.
	PriorityBackground Priority = iota
	// PriorityState: what fleetd's own state says matters after a restart,
	// when the live signals are gone: pods of fleets with Fleet MCP on,
	// running automation agents, the mic or output target.
	PriorityState
	// PriorityInteractive: a subscribed TUI, audio flowing when the session
	// dropped, a live MCP stream, or a job waiting on the agent.
	PriorityInteractive
)

// limiter bounds the bootstraps in flight on one connection, granting slots
// highest priority first, then in arrival order.
type limiter struct {
	mu      sync.Mutex
	max     int
	running int
	queue   waitQueue
	seq     uint64
}

func newLimiter(max int) *limiter {
	if max < 1 {
		max = 1
	}
	return &limiter{max: max}
}

type waiter struct {
	prio    Priority
	seq     uint64
	ready   chan struct{}
	index   int
	granted bool
}

// ticket is one bootstrap's claim on its connection's limiter. Every caller
// that joins the bootstrap raises it to its own priority, so a pod someone
// is waiting on interactively is not left queued at the priority of the
// background poke that started its bootstrap.
type ticket struct {
	mu   sync.Mutex
	prio Priority
	l    *limiter
	w    *waiter // while queued
}

func newTicket(prio Priority) *ticket { return &ticket{prio: prio} }

// raise lifts the ticket to prio (never lowers it), moving it up the queue
// when it is waiting.
func (t *ticket) raise(prio Priority) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prio <= t.prio {
		return
	}
	t.prio = prio
	if t.w != nil {
		t.l.raise(t.w, prio)
	}
}

// acquire waits, within ctx, for a slot; release gives it back.
func (l *limiter) acquire(ctx context.Context, prio Priority) (release func(), err error) {
	return l.acquireTicket(ctx, newTicket(prio))
}

// acquireTicket is acquire at the ticket's priority, which may rise while it
// waits.
func (l *limiter) acquireTicket(ctx context.Context, t *ticket) (release func(), err error) {
	// Lock order: the ticket, then the limiter (as raise takes them).
	t.mu.Lock()
	l.mu.Lock()
	if l.running < l.max && l.queue.Len() == 0 {
		l.running++
		l.mu.Unlock()
		t.mu.Unlock()
		return l.releaser(), nil
	}
	l.seq++
	w := &waiter{prio: t.prio, seq: l.seq, ready: make(chan struct{})}
	heap.Push(&l.queue, w)
	t.l, t.w = l, w
	l.mu.Unlock()
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.w = nil
		t.mu.Unlock()
	}()
	select {
	case <-w.ready:
		return l.releaser(), nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// Granted as it gave up: hand the slot on.
			l.mu.Unlock()
			l.releaser()()
			return nil, ctx.Err()
		}
		heap.Remove(&l.queue, w.index)
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *limiter) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			for l.running < l.max && l.queue.Len() > 0 {
				w := heap.Pop(&l.queue).(*waiter)
				w.granted = true
				l.running++
				close(w.ready)
			}
		})
	}
}

// raise lifts a queued waiter to prio; a granted or abandoned one is left
// alone.
func (l *limiter) raise(w *waiter, prio Priority) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w.granted || w.index < 0 || prio <= w.prio {
		return
	}
	w.prio = prio
	heap.Fix(&l.queue, w.index)
}

// inFlight is how many slots are taken.
func (l *limiter) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

type waitQueue []*waiter

func (q waitQueue) Len() int { return len(q) }
func (q waitQueue) Less(i, j int) bool {
	if q[i].prio != q[j].prio {
		return q[i].prio > q[j].prio
	}
	return q[i].seq < q[j].seq
}
func (q waitQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *waitQueue) Push(x any) {
	w := x.(*waiter)
	w.index = len(*q)
	*q = append(*q, w)
}
func (q *waitQueue) Pop() any {
	old := *q
	w := old[len(old)-1]
	*q = old[:len(old)-1]
	w.index = -1
	return w
}

// fullJitter is the wait before attempt n (1 for the first retry): a
// uniformly random share r (in [0, 1)) of base doubled per attempt, capped at
// limit, so pods that failed together retry spread out.
func fullJitter(n int, base, limit time.Duration, r float64) time.Duration {
	d := limit
	if n >= 1 && n <= 30 {
		if b := base << (n - 1); b > 0 && b < limit {
			d = b
		}
	}
	return time.Duration(r * float64(d))
}

// tokenBucket paces the fallback execs on one connection: each is an API
// server exec upgrade, which the agent session exists to avoid.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens a second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate float64, burst int, now func() time.Time) *tokenBucket {
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), now: now}
}

// wait takes a token, waiting within ctx for one.
func (b *tokenBucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := b.now()
		if !b.last.IsZero() {
			b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		t := time.NewTimer(need)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}
