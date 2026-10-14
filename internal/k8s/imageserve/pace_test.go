package imageserve

import (
	"context"
	"net"
	"testing"
	"time"
)

// simLink is an upload link with a send buffer, on a fake clock: a write
// returns at once while the buffer has room, and waits for it to drain at
// rate otherwise (a TCP socket in front of a slower link).
type simLink struct {
	clock       *fakeClock
	rate        float64 // bytes per second
	buf, level  float64
	at          time.Time
	sent        int64
	blockedTime time.Duration
}

func (l *simLink) drain() {
	el := l.clock.t.Sub(l.at).Seconds()
	l.level = max(0, l.level-el*l.rate)
	l.at = l.clock.t
}

func (l *simLink) write(n int) {
	l.drain()
	if over := l.level + float64(n) - l.buf; over > 0 {
		wait := time.Duration(over / l.rate * float64(time.Second))
		l.clock.advance(wait)
		l.blockedTime += wait
		l.drain()
	}
	l.level += float64(n)
	l.sent += int64(n)
}

// simConn is a net.Conn whose writes go to a simLink.
type simConn struct {
	net.Conn
	link *simLink
}

func (c simConn) Write(p []byte) (int, error) {
	c.link.write(len(p))
	return len(p), nil
}

func newSim(rate float64) (*fakeClock, *simLink, *Uplink) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	link := &simLink{clock: clock, rate: rate, buf: 512 << 10, at: clock.t}
	u := &Uplink{now: clock.now, sleep: func(_ context.Context, d time.Duration) error { clock.advance(d); return nil }}
	return clock, link, u
}

// pull sends for d of fake time through w, in 32 KiB writes, and returns the
// bytes the link sent meanwhile.
func pull(t *testing.T, clock *fakeClock, link *simLink, w interface{ Write([]byte) (int, error) }, d time.Duration) int64 {
	t.Helper()
	chunk := make([]byte, 32<<10)
	start, sent := clock.t, link.sent
	for clock.t.Sub(start) < d {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	return link.sent - sent
}

func TestUplinkMeasuresAndPaces(t *testing.T) {
	const tenMbit = 10e6 / 8
	clock, link, u := newSim(tenMbit)
	conn := u.Meter(simConn{link: link})
	end := u.begin(80)
	defer end()
	w := u.writer(context.Background(), conn, 80)

	// Unmeasured, the first seconds go unpaced and measure the link.
	pull(t, clock, link, w, 5*time.Second)
	if est := u.Rate(); est < 0.9*tenMbit || est > 1.1*tenMbit {
		t.Fatalf("estimate after 5s: %.0f B/s, want about %.0f", est, tenMbit)
	}

	// Paced at 80%: the link's buffer stays empty (writes do not block)
	// apart from the probes, and the long-run rate is about 80% plus the
	// probes' share.
	blockedBefore := link.blockedTime
	sent := pull(t, clock, link, w, 5*time.Minute)
	rate := float64(sent) / (5 * time.Minute).Seconds()
	if rate < 0.78*tenMbit || rate > 0.9*tenMbit {
		t.Fatalf("paced rate %.0f B/s, want about 80%% of %.0f (plus probes)", rate, tenMbit)
	}
	blocked := link.blockedTime - blockedBefore
	// Probes run 4s of every 34s at full rate, and the buffer they filled
	// drains under the pace after each; nothing else blocks.
	if blocked > time.Duration(float64(5*time.Minute)*(probeFor+1500*time.Millisecond).Seconds()/(probeEvery+probeFor).Seconds()) {
		t.Fatalf("writes blocked for %v of 5m; pacing must keep the queue empty between probes", blocked)
	}
	if est := u.Rate(); est < 0.9*tenMbit || est > 1.1*tenMbit {
		t.Fatalf("estimate after the paced stretch: %.0f B/s", est)
	}
	t.Logf("paced: %.0f B/s of a %.0f B/s link, blocked %v of 5m", rate, tenMbit, blocked)

	// The link halves (someone else uploads): paced writes start to block,
	// which measures it again.
	link.rate = tenMbit / 2
	pull(t, clock, link, w, 20*time.Second)
	if est := u.Rate(); est > 0.65*tenMbit {
		t.Fatalf("estimate 20s after the link halved: %.0f B/s, want about %.0f", est, tenMbit/2)
	}

	// The link recovers: a probe finds the room.
	link.rate = tenMbit
	pull(t, clock, link, w, 2*time.Minute)
	if est := u.Rate(); est < 0.85*tenMbit {
		t.Fatalf("estimate 2m after the link recovered: %.0f B/s", est)
	}
}

func TestUplinkIgnoresTheBufferFill(t *testing.T) {
	// A short burst into an empty buffer never blocks: it says nothing about
	// the link.
	clock, link, u := newSim(1 << 20)
	conn := u.Meter(simConn{link: link})
	for range 8 {
		_, _ = conn.Write(make([]byte, 32<<10))
		clock.advance(10 * time.Millisecond)
	}
	if est := u.Rate(); est != 0 {
		t.Fatalf("estimate from a buffered burst: %.0f", est)
	}
}

func TestUplinkPaceShares(t *testing.T) {
	clock, _, u := newSim(1 << 20)
	u.estimate = 1 << 20
	if r := u.pacedRate(clock.t); r != 0 {
		t.Fatalf("no pulls, rate %.0f", r)
	}
	endA := u.begin(80)
	endB := u.begin(50)
	if r := u.pacedRate(clock.t); r != 0.5*(1<<20) {
		t.Fatalf("pulls at 80%% and 50%%: rate %.0f, want the lower share", r)
	}
	endB()
	if r := u.pacedRate(clock.t); r != 0.8*(1<<20) {
		t.Fatalf("one pull at 80%%: rate %.0f", r)
	}
	endA()
	// Unpaced shares never wait.
	endC := u.begin(0)
	defer endC()
	if r := u.pacedRate(clock.t); r != 0 {
		t.Fatalf("an unpaced pull is paced at %.0f", r)
	}
	if err := u.wait(context.Background(), 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	// A tiny estimate still leaves minPacedRate.
	u.estimate = 1000
	endD := u.begin(10)
	defer endD()
	if r := u.pacedRate(clock.t); r != minPacedRate {
		t.Fatalf("rate %.0f, want the floor %d", r, minPacedRate)
	}
}

func TestUplinkWaitStopsWithTheContext(t *testing.T) {
	u := &Uplink{}
	u.estimate = 1024
	end := u.begin(50)
	defer end()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := u.wait(ctx, 10<<20, 50); err == nil {
		t.Fatal("a cancelled pull kept waiting")
	}
}
