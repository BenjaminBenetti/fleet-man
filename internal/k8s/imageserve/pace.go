package imageserve

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// pace.go paces registry uploads. Image layers go up the daemon host's
// upload link, which the main tunnel (MCP, remote gRPC, webhooks) shares, so
// a pull is held to a share of the measured uplink (80% by default, the
// connection's Image.UploadPacePercent): the link's queues stay short, the
// main tunnel's latency stays low and neither connection's keepalive waits
// behind a backlog of layer bytes.
//
// The uplink is measured on the dedicated registry connection (Meter wraps
// it): while its writes block, the link is the bottleneck and the bytes it
// accepts per second are what the link drains. Paced writes never block, so
// every probeEvery of paced transfer the pacing lifts for probeFor and the
// link is measured again (it may have more room now). A link that has
// shrunk below the paced rate makes paced writes block, which measures it at
// once.

const (
	// sampleWindow is one measurement.
	sampleWindow = time.Second
	// blockedWrite is how long a write must take to count as blocked by
	// the link.
	blockedWrite = 2 * time.Millisecond
	// idleReset ends a burst: after this long without writes the link's
	// buffers have drained, and the next measurement waits for a write to
	// block again (until then accepted bytes fill buffers, not the link).
	idleReset = 2 * time.Second
	// probeEvery and probeFor schedule the unpaced re-measurements.
	probeEvery = 30 * time.Second
	probeFor   = 4 * time.Second
	// paceBurst is the token bucket's depth.
	paceBurst = 256 << 10
	// minPacedRate is the least a paced pull gets, however small the
	// estimate.
	minPacedRate = 64 << 10
	// paceChunk is the largest write paced at once.
	paceChunk = 32 << 10
)

// Uplink measures the daemon host's upload rate and paces registry uploads
// to a share of it. The zero value is ready; one Uplink serves the daemon.
type Uplink struct {
	// now and sleep override the clock (tests).
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu sync.Mutex
	// estimate is the measured uplink in bytes per second; 0 until measured.
	estimate float64

	// The current measurement: armed once a write blocked since the last
	// idle stretch, from winStart on.
	armed      bool
	winStart   time.Time
	winBytes   int64
	winBlocked time.Duration
	lastWrite  time.Time

	// Pacing: the pulls running, by their share, and the token bucket.
	active      map[int]int
	tokens      float64
	tokensAt    time.Time
	pacedSince  time.Time
	probeUntil  time.Time
	lastPacedAt time.Time
	// settleUntil ends the stretch after a probe in which the buffers the
	// probe filled drain under paced writes: their blocking measures the
	// pace, not the link.
	settleUntil time.Time
}

func (u *Uplink) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

func (u *Uplink) pause(ctx context.Context, d time.Duration) error {
	if u.sleep != nil {
		return u.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Rate is the measured uplink in bytes per second (0 until measured).
func (u *Uplink) Rate() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.estimate
}

// Meter wraps the registry connection so its writes measure the uplink.
func (u *Uplink) Meter(c net.Conn) net.Conn { return &meteredConn{Conn: c, u: u} }

type meteredConn struct {
	net.Conn
	u *Uplink
}

func (c *meteredConn) Write(p []byte) (int, error) {
	start := c.u.clock()
	n, err := c.Conn.Write(p)
	c.u.observe(n, c.u.clock().Sub(start))
	return n, err
}

// observe records one write of n bytes that took took.
func (u *Uplink) observe(n int, took time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.clock()
	if !u.lastWrite.IsZero() && now.Sub(u.lastWrite) > idleReset {
		u.armed = false
	}
	u.lastWrite = now
	blocked := took >= blockedWrite
	if !u.armed {
		if blocked {
			u.armed, u.winStart, u.winBytes, u.winBlocked = true, now, 0, 0
		}
		return
	}
	u.winBytes += int64(n)
	if blocked {
		u.winBlocked += took
	}
	elapsed := now.Sub(u.winStart)
	if elapsed < sampleWindow {
		return
	}
	settling := now.After(u.probeUntil) && u.winStart.Before(u.settleUntil)
	if u.winBlocked*2 >= elapsed && !settling {
		sample := float64(u.winBytes) / elapsed.Seconds()
		if u.estimate == 0 {
			u.estimate = sample
		} else {
			u.estimate = 0.7*u.estimate + 0.3*sample
		}
	}
	u.winStart, u.winBytes, u.winBlocked = now, 0, 0
}

// begin registers a pull paced at pct percent of the uplink; the returned
// func ends it. Pulls running at once share one pace: the lowest share any
// of them asks for.
func (u *Uplink) begin(pct int) func() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.active == nil {
		u.active = map[int]int{}
	}
	u.active[pct]++
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.active[pct]--; u.active[pct] <= 0 {
			delete(u.active, pct)
		}
	}
}

// pacedRate is the bytes per second paced pulls may send now; 0 is unpaced.
// Callers hold u.mu.
func (u *Uplink) pacedRate(now time.Time) float64 {
	pct := 0
	for p, n := range u.active {
		if n > 0 && p > 0 && p < 100 && (pct == 0 || p < pct) {
			pct = p
		}
	}
	if pct == 0 || u.estimate == 0 {
		u.pacedSince = time.Time{}
		return 0
	}
	if now.Before(u.probeUntil) {
		return 0
	}
	if u.pacedSince.IsZero() || now.Sub(u.lastPacedAt) > idleReset {
		u.pacedSince = now
	}
	u.lastPacedAt = now
	if now.Sub(u.pacedSince) >= probeEvery {
		u.probeUntil = now.Add(probeFor)
		u.settleUntil = u.probeUntil.Add(idleReset)
		u.pacedSince = u.probeUntil
		u.armed = false
		return 0
	}
	return max(u.estimate*float64(pct)/100, minPacedRate)
}

// wait holds a write of n bytes of a pull paced at pct until the pace
// allows it.
func (u *Uplink) wait(ctx context.Context, n int, pct int) error {
	if pct <= 0 || pct >= 100 {
		return nil
	}
	u.mu.Lock()
	now := u.clock()
	rate := u.pacedRate(now)
	if rate == 0 {
		u.tokens, u.tokensAt = paceBurst, now
		u.mu.Unlock()
		return nil
	}
	if u.tokensAt.IsZero() {
		u.tokens, u.tokensAt = paceBurst, now
	}
	u.tokens = min(paceBurst, u.tokens+rate*now.Sub(u.tokensAt).Seconds())
	u.tokensAt = now
	u.tokens -= float64(n)
	var d time.Duration
	if u.tokens < 0 {
		d = time.Duration(-u.tokens / rate * float64(time.Second))
	}
	u.mu.Unlock()
	if d <= 0 {
		return nil
	}
	return u.pause(ctx, d)
}

// writer paces writes to w for a pull at pct percent.
func (u *Uplink) writer(ctx context.Context, w io.Writer, pct int) io.Writer {
	return &pacedWriter{ctx: ctx, w: w, u: u, pct: pct}
}

type pacedWriter struct {
	ctx context.Context
	w   io.Writer
	u   *Uplink
	pct int
	n   int64
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), paceChunk)]
		if err := p.u.wait(p.ctx, len(chunk), p.pct); err != nil {
			return written, err
		}
		n, err := p.w.Write(chunk)
		written += n
		p.n += int64(n)
		if err != nil {
			return written, err
		}
		b = b[len(chunk):]
	}
	return written, nil
}
