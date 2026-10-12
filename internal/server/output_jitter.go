package server

import (
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// jitterReportFrames is the most frames one "audio output jitter" line
// covers (a minute of 20ms frames): a long burst is reported a minute at a
// time.
const jitterReportFrames = 3000

// jitterMinFrames is the fewest frames worth a line (a second's).
const jitterMinFrames = 50

// frameJitter measures how late an audio source's frames reach the daemon
// against the pace they are made at, for fleet.log's "audio output jitter"
// lines: what a client's jitter buffer has to absorb (a cluster's API
// server, a slow link, a bulk transfer beside the audio). A burst's frames
// are due one interval apart; each frame's lateness is how far it arrived
// behind that schedule, set by the burst's earliest-arriving frame. It is
// not safe for concurrent use: one per stream.
type frameJitter struct {
	interval time.Duration
	start    time.Time       // the burst's first frame's arrival
	offsets  []time.Duration // each frame's arrival less its place in the schedule
}

func newFrameJitter(interval time.Duration) *frameJitter {
	return &frameJitter{interval: interval}
}

// arrived records a frame of the burst arriving at now; a gap of more than
// a second starts a new burst (the source paused), reporting the last.
func (j *frameJitter) arrived(key string, now time.Time) {
	if !j.start.IsZero() {
		if due := j.start.Add(time.Duration(len(j.offsets)) * j.interval); now.Sub(due) > time.Second {
			j.report(key)
		}
	}
	if j.start.IsZero() {
		j.start = now
	}
	j.offsets = append(j.offsets, now.Sub(j.start)-time.Duration(len(j.offsets))*j.interval)
	if len(j.offsets) >= jitterReportFrames {
		j.report(key)
	}
}

// report logs the burst's frames so far (when there are enough to judge)
// and starts the next burst.
func (j *frameJitter) report(key string) {
	if s, ok := j.summary(); ok {
		flog.Info("audio output jitter", "instance", key, "frames", s.frames, "p50_ms", s.p50.Milliseconds(), "p99_ms", s.p99.Milliseconds(), "max_ms", s.max.Milliseconds())
	}
	j.start, j.offsets = time.Time{}, j.offsets[:0]
}

// jitterSummary is a burst's lateness: its median, 99th percentile and
// worst frame.
type jitterSummary struct {
	frames        int
	p50, p99, max time.Duration
}

// summary is the burst's lateness so far; ok is false for too short a burst.
func (j *frameJitter) summary() (jitterSummary, bool) {
	if len(j.offsets) < jitterMinFrames {
		return jitterSummary{}, false
	}
	late := slices.Clone(j.offsets)
	best := slices.Min(late)
	for i := range late {
		late[i] -= best
	}
	slices.Sort(late)
	at := func(q float64) time.Duration { return late[min(len(late)-1, int(q*float64(len(late))))] }
	return jitterSummary{frames: len(late), p50: at(0.50), p99: at(0.99), max: late[len(late)-1]}, true
}
