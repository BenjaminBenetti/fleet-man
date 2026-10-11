package podwire

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// The bulk session shares the pod's network path (the daemon host's uplink,
// the apiserver, Konnectivity) with the agent session's PCM. It is paced only
// while audio is actually flowing on that pod, and then only once PCM shows it
// is being crowded out: unpaced until a PCM frame arrives more than
// LateThreshold late, then halved on each late frame, recovering by PaceStep a
// second, never below PaceFloor. An open but silent audio stream (audio output
// is on by default, so one is open to every running instance while a client
// is attached) never counts.
const (
	// PaceFloor is the slowest a paced bulk session goes.
	PaceFloor = 2 << 20
	// PaceStep is the additive recovery, per second without a late frame.
	PaceStep = 1 << 20
	// LateThreshold is how late a PCM frame must arrive to count as late.
	LateThreshold = 60 * time.Millisecond
	// OutputAudibleWindow is how long after the output source's last audible
	// frame audio still counts as flowing.
	OutputAudibleWindow = 2 * time.Second

	// paceHoldoff is the least time between two halvings, so one burst of
	// late frames halves the rate once.
	paceHoldoff = 250 * time.Millisecond
	// paceChunk is the largest write a paced conn hands on at once.
	paceChunk = 64 << 10
	// measureWindow is the throughput measurement's window.
	measureWindow = 250 * time.Millisecond
)

// AudioActivity says whether audio is actually flowing on one pod: the
// microphone has demand (a recorder is listening), or the output source
// delivered audible frames within OutputAudibleWindow. It is safe for
// concurrent use.
type AudioActivity struct {
	mu          sync.Mutex
	micDemand   int
	lastAudible time.Time
	now         func() time.Time
}

// SetMicDemand records the pod's current microphone demand.
func (a *AudioActivity) SetMicDemand(n int) {
	a.mu.Lock()
	a.micDemand = n
	a.mu.Unlock()
}

// OutputAudible records that the output source just delivered a frame that
// was not silence.
func (a *AudioActivity) OutputAudible() {
	a.mu.Lock()
	a.lastAudible = a.clock()
	a.mu.Unlock()
}

// Flowing reports whether audio is flowing on the pod.
func (a *AudioActivity) Flowing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.micDemand > 0 {
		return true
	}
	return !a.lastAudible.IsZero() && a.clock().Sub(a.lastAudible) < OutputAudibleWindow
}

func (a *AudioActivity) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// Pacer paces one pod's bulk session. Wrap its transport before the bulk
// session's yamux; feed Late from the pod's PCM frames (FrameClock). It is safe
// for concurrent use.
type Pacer struct {
	flowing func() bool
	now     func() time.Time
	sleep   func(time.Duration)

	// onPaced is told each time pacing starts (the paced rate) or ends (0).
	onPaced atomic.Pointer[func(rate float64)]

	mu sync.Mutex
	// rate is the paced rate in bytes a second; 0 is unpaced.
	rate float64
	// target is the throughput pacing began from: recovering back to it
	// ends pacing.
	target      float64
	lastCut     time.Time
	lastRecover time.Time
	// The token bucket.
	tokens   float64
	lastFill time.Time
	// The throughput measurement.
	measured float64
	winStart time.Time
	winBytes int64
}

// NewPacer paces while flowing reports audio flowing on the pod (an
// AudioActivity's Flowing).
func NewPacer(flowing func() bool) *Pacer {
	return &Pacer{flowing: flowing, now: time.Now, sleep: time.Sleep}
}

// OnPaced sets fn to be told, outside the pacer's lock, each time pacing
// starts (with the paced rate) and each time it ends (0): when audio stops
// flowing, or the rate recovered to where pacing began.
func (p *Pacer) OnPaced(fn func(rate float64)) {
	if fn == nil {
		p.onPaced.Store(nil)
		return
	}
	p.onPaced.Store(&fn)
}

// notify tells onPaced of a start (rate > 0) or an end (0).
func (p *Pacer) notify(rate float64) {
	if fn := p.onPaced.Load(); fn != nil {
		(*fn)(rate)
	}
}

// Late records that a PCM frame on the pod arrived more than LateThreshold
// late. While audio flows it starts pacing at half the measured throughput, or
// halves the paced rate, at most once per paceHoldoff and never below
// PaceFloor.
func (p *Pacer) Late() {
	if !p.flowing() {
		return
	}
	p.mu.Lock()
	now := p.now()
	if !p.lastCut.IsZero() && now.Sub(p.lastCut) < paceHoldoff {
		p.mu.Unlock()
		return
	}
	started := p.rate == 0
	if started {
		p.target = max(p.measured, 2*PaceFloor)
		p.rate = max(p.measured/2, PaceFloor)
		p.tokens, p.lastFill = 0, now
	} else {
		p.rate = max(p.rate/2, PaceFloor)
	}
	p.lastCut, p.lastRecover = now, now
	rate := p.rate
	p.mu.Unlock()
	if started {
		p.notify(rate)
	}
}

// Rate is the current paced rate in bytes a second; 0 is unpaced.
func (p *Pacer) Rate() float64 {
	p.mu.Lock()
	if p.rate > 0 && !p.flowing() {
		// Audio stopped: unpaced, as take finds it (the next time audio
		// flows starts unpaced).
		p.rate = 0
		p.mu.Unlock()
		p.notify(0)
		return 0
	}
	ended := p.recoverLocked(p.now())
	rate := p.rate
	p.mu.Unlock()
	if ended {
		p.notify(0)
	}
	return rate
}

// take accounts for n bytes crossing the bulk session, sleeping first when
// paced.
func (p *Pacer) take(n int) {
	p.mu.Lock()
	now := p.now()
	p.measureLocked(now, n)
	ended := false
	if p.rate > 0 && !p.flowing() {
		// Audio stopped: back to unpaced at once, so the next time it
		// flows starts unpaced too.
		p.rate, ended = 0, true
	}
	if p.recoverLocked(now) {
		ended = true
	}
	if p.rate == 0 {
		p.mu.Unlock()
		if ended {
			p.notify(0)
		}
		return
	}
	burst := max(p.rate/10, paceChunk)
	p.tokens = min(p.tokens+p.rate*now.Sub(p.lastFill).Seconds(), burst)
	p.lastFill = now
	p.tokens -= float64(n)
	var wait time.Duration
	if p.tokens < 0 {
		wait = time.Duration(-p.tokens / p.rate * float64(time.Second))
	}
	p.mu.Unlock()
	if wait > 0 {
		p.sleep(wait)
	}
}

// recoverLocked adds PaceStep for each whole second since the last cut or
// step; reaching the target ends pacing, which it reports.
func (p *Pacer) recoverLocked(now time.Time) bool {
	if p.rate == 0 {
		return false
	}
	steps := int(now.Sub(p.lastRecover) / time.Second)
	if steps <= 0 {
		return false
	}
	p.rate += float64(steps) * PaceStep
	p.lastRecover = p.lastRecover.Add(time.Duration(steps) * time.Second)
	if p.rate >= p.target {
		p.rate = 0
		return true
	}
	return false
}

// measureLocked keeps a smoothed throughput over measureWindow windows.
func (p *Pacer) measureLocked(now time.Time, n int) {
	if p.winStart.IsZero() {
		p.winStart = now
	}
	p.winBytes += int64(n)
	if elapsed := now.Sub(p.winStart); elapsed >= measureWindow {
		inst := float64(p.winBytes) / elapsed.Seconds()
		if p.measured == 0 {
			p.measured = inst
		} else {
			p.measured = (p.measured + inst) / 2
		}
		p.winStart, p.winBytes = now, 0
	}
}

// Wrap paces rw: writes before they go out, reads after they come in (which
// holds the sender back through yamux's flow control).
func (p *Pacer) Wrap(rw io.ReadWriteCloser) io.ReadWriteCloser {
	return &pacedConn{rw: rw, p: p}
}

type pacedConn struct {
	rw io.ReadWriteCloser
	p  *Pacer
}

func (c *pacedConn) Read(b []byte) (int, error) {
	n, err := c.rw.Read(b)
	if n > 0 {
		c.p.take(n)
	}
	return n, err
}

func (c *pacedConn) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n := min(len(b), paceChunk)
		c.p.take(n)
		w, err := c.rw.Write(b[:n])
		written += w
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

func (c *pacedConn) Close() error { return c.rw.Close() }

// FrameClock tells late PCM frames: frames due every interval (40ms for the
// microphone's), each counted late when it arrives more than LateThreshold
// after it was due. It is not safe for concurrent use: one per stream.
type FrameClock struct {
	interval  time.Duration
	threshold time.Duration
	onLate    func()
	next      time.Time
}

// NewFrameClock calls onLate (a Pacer's Late) for each late frame.
func NewFrameClock(interval time.Duration, onLate func()) *FrameClock {
	return &FrameClock{interval: interval, threshold: LateThreshold, onLate: onLate}
}

// SetThreshold sets how late a frame must arrive to count as late (default
// LateThreshold); a threshold <= 0 is ignored.
func (c *FrameClock) SetThreshold(d time.Duration) {
	if d > 0 {
		c.threshold = d
	}
}

// Arrived records a frame's arrival at now and reports whether it was late.
// A gap of more than a second restarts the clock (the stream paused).
func (c *FrameClock) Arrived(now time.Time) bool {
	if c.next.IsZero() || now.Sub(c.next) > time.Second {
		c.next = now.Add(c.interval)
		return false
	}
	if now.Sub(c.next) > c.threshold {
		c.next = now.Add(c.interval)
		if c.onLate != nil {
			c.onLate()
		}
		return true
	}
	c.next = c.next.Add(c.interval)
	return false
}
