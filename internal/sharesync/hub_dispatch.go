package sharesync

import (
	"context"
	"io"
	"sync"
	"time"
)

// hubSide is an attached side.
type hubSide struct {
	info   SideInfo
	peer   Peer
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	// started counts the running dispatcher.
	started sync.WaitGroup

	mu sync.Mutex
	// downUntil holds dispatch back after the side could not be reached.
	downUntil time.Time
	downN     int
	downErr   string
}

// poke wakes the side's dispatcher.
func (s *hubSide) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// sideRoot is the hub's view of one side in one root.
type sideRoot struct {
	side *hubSide
	// held is the rev the side holds of each path it holds; 0 is content
	// at a rev the hub does not know.
	held map[string]uint64
	// dirty are the paths to look at for the side, since when.
	dirty map[string]time.Time
	// inflight are the paths in a batch now, with the side's report count
	// for each when it was sent.
	inflight map[string]uint64
	// reports counts the side's reports of each path, so a result that
	// predates the side's report of its change does not back the path off.
	reports map[string]uint64
	backoff map[string]retry
	// blocked are the paths the side refused at a rev.
	blocked map[string]refusal
	// paused is why the side cannot take files now (its disk is full);
	// they are tried again from retryAt.
	paused  string
	retryAt time.Time
	lastErr string
}

// retry is a path's backoff.
type retry struct {
	until time.Time
	n     int
}

// refusal is a side's refusal of a path's state.
type refusal struct {
	rev    uint64
	reason string
}

func newSideRoot(s *hubSide) *sideRoot {
	return &sideRoot{
		side:     s,
		held:     map[string]uint64{},
		dirty:    map[string]time.Time{},
		inflight: map[string]uint64{},
		reports:  map[string]uint64{},
		backoff:  map[string]retry{},
		blocked:  map[string]refusal{},
	}
}

// mark marks p to be looked at for the side and wakes it.
func (sr *sideRoot) mark(p string, now time.Time) {
	if _, ok := sr.dirty[p]; !ok {
		sr.dirty[p] = now
	}
	sr.side.poke()
}

// later marks p again after a backoff from base, doubling to 30s.
func (sr *sideRoot) later(p string, now time.Time, base time.Duration) {
	r := sr.backoff[p]
	r.n++
	d := base << min(r.n-1, 10)
	r.until = now.Add(min(d, 30*time.Second))
	sr.backoff[p] = r
	if _, ok := sr.dirty[p]; !ok {
		sr.dirty[p] = now
	}
}

// run is a side's dispatcher: it sends the side what it lacks, lane by lane,
// until the side is detached.
func (h *Hub) run(s *hubSide) {
	defer s.started.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next := h.dispatch(s)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !next.IsZero() {
			timer.Reset(max(time.Until(next), time.Millisecond))
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		}
	}
}

// dispatch sends a side every batch due now, and returns when to look again
// (zero for only when woken).
func (h *Hub) dispatch(s *hubSide) time.Time {
	var next time.Time
	for s.ctx.Err() == nil {
		s.mu.Lock()
		down := s.downUntil
		s.mu.Unlock()
		if now := h.now(); now.Before(down) {
			return earliest(next, down)
		}
		sent := false
		for _, root := range h.order {
			batches, wait := root.collect(s, h.now(), h.flushing.Load() > 0)
			next = earliest(next, wait)
			for i, b := range batches {
				results, err := s.peer.Apply(s.ctx, b)
				root.finish(s, b, results, err)
				sent = true
				if err != nil {
					for _, rest := range batches[i+1:] {
						root.finish(s, rest, nil, err)
					}
					h.unreachable(s, err)
					break
				}
				s.mu.Lock()
				s.downN, s.downErr = 0, ""
				s.mu.Unlock()
			}
		}
		if !sent {
			return next
		}
	}
	return time.Time{}
}

// unreachable backs a side off after it could not be reached.
func (h *Hub) unreachable(s *hubSide, err error) {
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downN++
	d := min(250*time.Millisecond<<min(s.downN-1, 10), 30*time.Second)
	s.downUntil = h.now().Add(d)
	if s.downErr == "" {
		h.log.Warn("sharesync: side unreachable", "side", s.info.ID, "err", err)
	}
	s.downErr = err.Error()
}

// earliest returns the earlier non-zero time.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	}
	return a
}

// opFor returns the op that brings the side to p's state, or false when it
// holds it.
func (root *hubRoot) opFor(sr *sideRoot, p string) (Op, bool) {
	k, _, ok := root.keyFor(p, sr.side.info.logins())
	held, holds := sr.held[p]
	if !ok {
		return Op{}, false
	}
	e := root.entries[k]
	switch {
	case e == nil:
		if holds {
			// A delete the hub no longer remembers.
			return Op{Path: p, Rev: root.forgot, Entry: Entry{Kind: Gone}}, true
		}
	case e.Entry.Live():
		if !holds || held != e.Rev {
			return Op{Path: p, Rev: e.Rev, Entry: e.Entry}, true
		}
	case holds:
		return Op{Path: p, Rev: e.Rev, Entry: e.Entry}, true
	}
	return Op{}, false
}

// laneFor returns the lane an op travels on.
func (root *hubRoot) laneFor(op Op) Lane {
	if m, ok := root.merger(op.Path); ok {
		if lane := m.Lane(); lane != LaneAuto {
			return lane
		}
	}
	if op.Entry.Kind == File && op.Entry.Size > BulkSize {
		return LaneBulk
	}
	return LaneNormal
}

// delay returns how long a change waits on lane.
func (h *Hub) delay(lane Lane) time.Duration {
	switch lane {
	case LanePriority:
		return 0
	case LaneBulk:
		return h.opts.BulkDelay
	}
	return h.opts.NormalDelay
}

// lanes is the order batches go in.
var lanes = []Lane{LanePriority, LaneNormal, LaneBulk}

// collect returns the batches due for a side now, and when the next is due.
// The ops it returns are in flight until finish.
func (root *hubRoot) collect(s *hubSide, now time.Time, flush bool) ([]Batch, time.Time) {
	root.mu.Lock()
	defer root.mu.Unlock()
	sr := root.sides[s.info.ID]
	if sr == nil || root.broken != nil {
		return nil, time.Time{}
	}
	var next time.Time
	byLane := map[Lane][]Op{}
	for p, since := range sr.dirty {
		if _, busy := sr.inflight[p]; busy {
			continue
		}
		op, ok := root.opFor(sr, p)
		if !ok {
			delete(sr.dirty, p)
			delete(sr.backoff, p)
			continue
		}
		if r, ok := sr.blocked[p]; ok {
			if r.rev == op.Rev {
				delete(sr.dirty, p)
				continue
			}
			delete(sr.blocked, p)
		}
		if r, ok := sr.backoff[p]; ok && now.Before(r.until) {
			next = earliest(next, r.until)
			continue
		}
		if sr.paused != "" && op.Entry.Kind == File && now.Before(sr.retryAt) {
			next = earliest(next, sr.retryAt)
			continue
		}
		lane := root.laneFor(op)
		if due := since.Add(root.hub.delay(lane)); !flush && now.Before(due) {
			next = earliest(next, due)
			continue
		}
		byLane[lane] = append(byLane[lane], op)
	}
	var batches []Batch
	for _, lane := range lanes {
		ops := byLane[lane]
		sortOps(ops)
		for len(ops) > 0 {
			n := min(len(ops), root.hub.opts.MaxBatch)
			chunk := ops[:n:n]
			ops = ops[n:]
			for _, op := range chunk {
				sr.inflight[op.Path] = sr.reports[op.Path]
				delete(sr.dirty, op.Path)
			}
			batches = append(batches, Batch{Root: root.name, Epoch: root.epoch, Lane: lane, Ops: chunk, Content: root.contentFor(s)})
		}
	}
	return batches, next
}

// contentFor returns the ContentFunc of a side's batches: the content of the
// state an op names, from the hub's blobs or a side that holds it.
func (root *hubRoot) contentFor(s *hubSide) ContentFunc {
	return func(ctx context.Context, op Op, offset int64) (io.ReadCloser, error) {
		root.mu.Lock()
		k, _, ok := root.keyFor(op.Path, s.info.logins())
		e := root.entries[k]
		root.mu.Unlock()
		if !ok || e == nil || e.Rev != op.Rev {
			return nil, ErrStale
		}
		return root.hub.openContent(ctx, root, k, e.Entry, e.Rev, offset, s.info.ID)
	}
}

// finish takes a side's results for a batch (err: it could not be reached).
func (root *hubRoot) finish(s *hubSide, b Batch, results []Result, err error) {
	root.mu.Lock()
	defer root.mu.Unlock()
	sr := root.sides[s.info.ID]
	if sr == nil {
		return
	}
	now := root.hub.now()
	sentAt := map[string]uint64{}
	for _, op := range b.Ops {
		sentAt[op.Path] = sr.inflight[op.Path]
		delete(sr.inflight, op.Path)
	}
	if err != nil {
		for _, op := range b.Ops {
			if op.Entry.Live() {
				// Unanswered, it may have landed: the side may hold this
				// state now, at a rev the hub cannot count on, so a later
				// delete of the path is sent to it too.
				sr.held[op.Path] = 0
			}
			if _, ok := sr.dirty[op.Path]; !ok {
				sr.dirty[op.Path] = now
			}
		}
		sr.lastErr = err.Error()
		return
	}
	got := map[string]Result{}
	for _, r := range results {
		got[r.Path] = r
	}
	log := root.hub.log
	for _, op := range b.Ops {
		r, ok := got[op.Path]
		if !ok || r.Rev != op.Rev {
			r = Result{Status: Failed, Reason: "no result"}
		}
		log.Debug("sharesync: op", "root", root.name, "side", s.info.ID, "path", op.Path, "rev", op.Rev, "kind", op.Entry.Kind, "status", r.Status, "reason", r.Reason)
		switch r.Status {
		case Applied:
			if op.Entry.Live() {
				if held, ok := sr.held[op.Path]; !ok || held <= op.Rev {
					sr.held[op.Path] = op.Rev
				}
			} else {
				delete(sr.held, op.Path)
			}
			delete(sr.backoff, op.Path)
			if _, again := sr.dirty[op.Path]; !again {
				// Done with the path: a send later counts reports afresh.
				delete(sr.reports, op.Path)
			}
			if op.Entry.Kind == File && sr.paused != "" {
				log.Info("sharesync: side has room again", "side", s.info.ID, "root", root.name)
				sr.paused, sr.retryAt = "", time.Time{}
			}
		case Changed:
			// The side holds a change of its own there: its report decides.
			if sr.reports[op.Path] != sentAt[op.Path] {
				// It has reported since: look again now.
				sr.mark(op.Path, now)
				continue
			}
			// Not yet: the side reports within its debounce, or has yet to
			// take the hub's answer to the report it made.
			sr.later(op.Path, now, 20*time.Millisecond)
		case Stale:
			// Not yet: the side reports within its debounce, or has yet to
			// take the hub's answer to the report it made.
			sr.later(op.Path, now, 20*time.Millisecond)
		case NoSpace:
			if sr.paused == "" {
				log.Warn("sharesync: sync paused", "side", s.info.ID, "root", root.name, "reason", r.Reason)
			}
			sr.paused = r.Reason
			if sr.paused == "" {
				sr.paused = "disk full"
			}
			sr.retryAt = now.Add(root.hub.opts.PauseRetry)
			// Whatever the op (a folder needs room too), it waits for the
			// retry with the files.
			sr.backoff[op.Path] = retry{until: sr.retryAt}
			sr.mark(op.Path, now)
		case Refused:
			log.Warn("sharesync: side refused a change", "side", s.info.ID, "root", root.name, "path", op.Path, "reason", r.Reason)
			sr.blocked[op.Path] = refusal{rev: op.Rev, reason: r.Reason}
		default:
			sr.lastErr = op.Path + ": " + r.Reason
			sr.later(op.Path, now, time.Second)
		}
	}
}
