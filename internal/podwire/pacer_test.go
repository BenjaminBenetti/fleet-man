package podwire

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// fakeClock is a clock tests move by hand; sleeping advances it.
type fakeClock struct {
	t     time.Time
	slept time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(d time.Duration) {
	c.slept += d
	c.t = c.t.Add(d)
}

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestAudioActivity(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	a := &AudioActivity{now: clock.now}
	if a.Flowing() {
		t.Fatal("flowing with no demand and nothing audible")
	}
	a.SetMicDemand(1)
	if !a.Flowing() {
		t.Fatal("mic demand is audio flowing")
	}
	a.SetMicDemand(0)
	a.OutputAudible()
	if !a.Flowing() {
		t.Fatal("audible output is audio flowing")
	}
	clock.advance(OutputAudibleWindow - time.Millisecond)
	if !a.Flowing() {
		t.Fatal("still within the audible window")
	}
	clock.advance(2 * time.Millisecond)
	if a.Flowing() {
		t.Fatal("an open but silent output stream counts as flowing")
	}
}

// newTestPacer is a pacer on a fake clock that says audio flows while
// *flowing.
func newTestPacer(flowing *bool) (*Pacer, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	p := NewPacer(func() bool { return *flowing })
	p.now, p.sleep = clock.now, clock.sleep
	return p, clock
}

// pump sends n bytes through p in 64KiB takes, spread over d of wall time on
// the fake clock, the way a fast bulk copy would.
func pump(p *Pacer, clock *fakeClock, n int, d time.Duration) {
	steps := n / paceChunk
	for range steps {
		clock.advance(d / time.Duration(steps))
		p.take(paceChunk)
	}
}

func TestPacerNeverPacesWithoutAudio(t *testing.T) {
	flowing := false
	p, clock := newTestPacer(&flowing)
	pump(p, clock, 64<<20, time.Second)
	for range 10 {
		p.Late()
	}
	pump(p, clock, 64<<20, time.Second)
	if p.Rate() != 0 || clock.slept != 0 {
		t.Fatalf("paced while no audio flows: rate %v, slept %v", p.Rate(), clock.slept)
	}
}

func TestPacerUnpacedUntilPCMIsLate(t *testing.T) {
	flowing := true
	p, clock := newTestPacer(&flowing)
	pump(p, clock, 64<<20, time.Second)
	if p.Rate() != 0 || clock.slept != 0 {
		t.Fatalf("audio flowing but no late frame: rate %v, slept %v", p.Rate(), clock.slept)
	}
}

func TestPacerHalvesOnLateFramesAndRecovers(t *testing.T) {
	flowing := true
	p, clock := newTestPacer(&flowing)
	// ~64 MiB/s measured.
	pump(p, clock, 64<<20, time.Second)
	p.Late()
	first := p.Rate()
	if first < 30<<20 || first > 34<<20 {
		t.Fatalf("first cut %v B/s, want about half of 64 MiB/s", first)
	}
	p.Late() // within the holdoff: ignored
	if p.Rate() != first {
		t.Fatalf("two cuts within the holdoff: %v", p.Rate())
	}
	for range 10 {
		clock.advance(paceHoldoff)
		p.Late()
	}
	if p.Rate() != PaceFloor {
		t.Fatalf("rate %v after many cuts, want the floor %v", p.Rate(), PaceFloor)
	}
	// Paced: 4 MiB at the floor takes about 2s of sleeping.
	clock.slept = 0
	for range (4 << 20) / paceChunk {
		p.take(paceChunk)
	}
	if clock.slept < 1500*time.Millisecond || clock.slept > 2500*time.Millisecond {
		t.Fatalf("4 MiB at the 2 MiB/s floor slept %v", clock.slept)
	}
	// Additive recovery, one PaceStep a second, until the target ends
	// pacing.
	before := p.Rate()
	clock.advance(3 * time.Second)
	if got := p.Rate(); got != before+3*PaceStep && got != 0 {
		t.Fatalf("after 3s: %v, want %v", got, before+3*PaceStep)
	}
	clock.advance(time.Minute)
	if p.Rate() != 0 {
		t.Fatalf("still paced after recovering past the target: %v", p.Rate())
	}
}

func TestPacerStopsWhenAudioStops(t *testing.T) {
	flowing := true
	p, clock := newTestPacer(&flowing)
	pump(p, clock, 16<<20, time.Second)
	p.Late()
	if p.Rate() == 0 {
		t.Fatal("not paced after a late frame")
	}
	flowing = false
	if p.Rate() != 0 {
		t.Fatal("paced after audio stopped")
	}
	clock.slept = 0
	pump(p, clock, 16<<20, time.Second)
	if clock.slept != 0 {
		t.Fatalf("slept %v with no audio", clock.slept)
	}
	// The next time audio flows starts unpaced too.
	flowing = true
	pump(p, clock, 16<<20, time.Second)
	if clock.slept != 0 || p.Rate() != 0 {
		t.Fatalf("audio flowing again started paced: %v", p.Rate())
	}
}

func TestPacedConnPassesBytes(t *testing.T) {
	flowing := false
	p, _ := newTestPacer(&flowing)
	var out bytes.Buffer
	conn := p.Wrap(rwc{Reader: bytes.NewReader(bytes.Repeat([]byte("r"), 300<<10)), Writer: &out})
	data := bytes.Repeat([]byte("w"), 300<<10)
	if n, err := conn.Write(data); err != nil || n != len(data) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("write %d %v", n, err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || len(got) != 300<<10 {
		t.Fatalf("read %d %v", len(got), err)
	}
}

type rwc struct {
	io.Reader
	io.Writer
}

func (rwc) Close() error { return nil }

func TestFrameClock(t *testing.T) {
	late := 0
	c := NewFrameClock(40*time.Millisecond, func() { late++ })
	start := time.Unix(1000, 0)
	at := start
	for i := range 10 {
		if c.Arrived(at) {
			t.Fatalf("frame %d on time counted late", i)
		}
		at = at.Add(40 * time.Millisecond)
	}
	// 30ms of jitter is fine; 70ms is late.
	if c.Arrived(at.Add(30 * time.Millisecond)) {
		t.Fatal("30ms of jitter counted late")
	}
	at = at.Add(40 * time.Millisecond)
	if !c.Arrived(at.Add(70 * time.Millisecond)) {
		t.Fatal("70ms late not counted")
	}
	if late != 1 {
		t.Fatalf("onLate called %d times", late)
	}
	// After a pause the clock restarts instead of counting every frame late.
	if c.Arrived(at.Add(5 * time.Second)) {
		t.Fatal("a resumed stream's first frame counted late")
	}
}

// OnPaced hears each start and each end of pacing, once: what fleetd logs so
// a paced bulk session shows in fleet.log.
func TestPacerTellsWhenPacingStartsAndEnds(t *testing.T) {
	flowing := true
	p, clock := newTestPacer(&flowing)
	var told []float64
	p.OnPaced(func(rate float64) { told = append(told, rate) })
	pump(p, clock, 16<<20, time.Second)
	if len(told) != 0 {
		t.Fatalf("told %v before any late frame", told)
	}
	p.Late()
	clock.advance(paceHoldoff)
	p.Late() // a further cut is no new start
	if len(told) != 1 || told[0] <= 0 {
		t.Fatalf("told %v, want one start with the paced rate", told)
	}
	flowing = false
	p.take(paceChunk)
	p.take(paceChunk)
	if len(told) != 2 || told[1] != 0 {
		t.Fatalf("told %v, want the end once audio stopped", told)
	}

	// Recovering to where pacing began ends it too.
	flowing = true
	pump(p, clock, 16<<20, time.Second)
	p.Late()
	clock.advance(time.Minute)
	_ = p.Rate()
	if len(told) != 4 || told[2] <= 0 || told[3] != 0 {
		t.Fatalf("told %v, want a second start and its end on recovery", told)
	}
	p.OnPaced(nil)
	pump(p, clock, 16<<20, time.Second)
	p.Late()
	if len(told) != 4 {
		t.Fatalf("told %v after OnPaced(nil)", told)
	}
}

func TestFrameClockThreshold(t *testing.T) {
	late := 0
	c := NewFrameClock(20*time.Millisecond, func() { late++ })
	c.SetThreshold(time.Millisecond)
	c.SetThreshold(0) // ignored
	at := time.Unix(1000, 0)
	c.Arrived(at)
	if !c.Arrived(at.Add(25 * time.Millisecond)) {
		t.Fatal("5ms late past a 1ms threshold not counted")
	}
	if late != 1 {
		t.Fatalf("onLate called %d times", late)
	}
}
