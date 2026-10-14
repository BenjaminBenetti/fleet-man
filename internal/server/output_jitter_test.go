package server

import (
	"testing"
	"time"
)

// A burst's lateness is measured against its earliest-arriving frame's pace:
// frames on time are 0 late, a stall shows as the frames it held up, and a
// frame that comes in early sets the schedule for all of them.
func TestFrameJitter(t *testing.T) {
	const interval = 20 * time.Millisecond
	t0 := time.Unix(1700000000, 0)
	at := func(i int, extra time.Duration) time.Time { return t0.Add(time.Duration(i)*interval + extra) }
	cases := []struct {
		name          string
		late          func(i int) time.Duration // frame i's delay
		frames        int
		p50, p99, max time.Duration
	}{
		{name: "on time", late: func(int) time.Duration { return 0 }, frames: 200},
		{name: "a stall", frames: 200, late: func(i int) time.Duration {
			// Frames 100-104 held up behind a bulk write, then all at once.
			if i >= 100 && i < 105 {
				return time.Duration(105-i) * interval
			}
			return 0
		}, p99: 80 * time.Millisecond, max: 100 * time.Millisecond},
		{name: "the first frame was the late one", frames: 200, late: func(i int) time.Duration {
			if i == 0 {
				return 0
			}
			return -30 * time.Millisecond // every later one came 30ms earlier
		}, max: 30 * time.Millisecond},
		{name: "steady 40ms of jitter", frames: 400, late: func(i int) time.Duration {
			return time.Duration(i%2) * 40 * time.Millisecond
		}, p50: 40 * time.Millisecond, p99: 40 * time.Millisecond, max: 40 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newFrameJitter(interval)
			for i := range tc.frames {
				j.arrived("fleet/inst", at(i, tc.late(i)))
			}
			got, ok := j.summary()
			if !ok || got.frames != tc.frames || got.p50 != tc.p50 || got.p99 != tc.p99 || got.max != tc.max {
				t.Fatalf("summary %+v %v, want %d frames, p50 %v p99 %v max %v", got, ok, tc.frames, tc.p50, tc.p99, tc.max)
			}
		})
	}
}

// A pause of more than a second starts a new burst; a burst too short to
// judge gives no summary; a long one is reported (and restarted) every
// jitterReportFrames frames.
func TestFrameJitterBursts(t *testing.T) {
	const interval = 20 * time.Millisecond
	j := newFrameJitter(interval)
	now := time.Unix(1700000000, 0)
	for range 10 {
		j.arrived("fleet/inst", now)
		now = now.Add(interval)
	}
	if _, ok := j.summary(); ok {
		t.Fatal("10 frames gave a summary")
	}
	now = now.Add(5 * time.Second)
	for range 60 {
		j.arrived("fleet/inst", now)
		now = now.Add(interval)
	}
	if s, ok := j.summary(); !ok || s.frames != 60 || s.max != 0 {
		t.Fatalf("after a pause: %+v %v, want the 60 frames of the new burst, none late", s, ok)
	}
	for range jitterReportFrames {
		j.arrived("fleet/inst", now)
		now = now.Add(interval)
	}
	if s, ok := j.summary(); !ok || s.frames != 60 {
		t.Fatalf("a long burst: %+v %v, want it reported at %d frames and 60 since", s, ok, jitterReportFrames)
	}
}
