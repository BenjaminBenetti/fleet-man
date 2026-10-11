package sharesync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// HubOptions configure a Hub. Zero values take the defaults.
type HubOptions struct {
	// Policies implements the table's policies; nil is DefaultPolicies.
	// OpenHub refuses a root whose policies are not all implemented.
	Policies Policies
	// Logger gets the hub's log; nil discards it.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NormalDelay and BulkDelay are how long a change waits, from when it
	// is first due, before it travels on the normal and bulk lanes: 100ms
	// and 1s.
	NormalDelay, BulkDelay time.Duration
	// PauseRetry is how often a side paused for want of space is tried
	// again: 10s.
	PauseRetry time.Duration
	// TombstoneTTL is how long a delete is remembered once no attached
	// side holds the path: 24h.
	TombstoneTTL time.Duration
	// MaxConflicts is how many conflict copies are kept: 200.
	MaxConflicts int
	// MaxBatch is the most ops in one batch: 256.
	MaxBatch int
	// MinFree is the fraction of the daemon host's disk the hub keeps free
	// when it keeps a conflict copy: 5%. Space reports it; nil is statfs.
	MinFree float64
	Space   SpaceFunc
}

// Hub is the sequencer of a fleet's shared state: sides report their
// changes to it, and it decides each by the path's policy and sends the
// outcome to every side that should have it.
type Hub struct {
	dir       string
	table     syncpolicy.Table
	policies  Policies
	opts      HubOptions
	log       *slog.Logger
	conflicts *conflictStore

	mu     sync.Mutex
	roots  map[string]*hubRoot
	order  []*hubRoot
	sides  map[string]*hubSide
	closed bool

	flushing atomic.Int32
	// hostFull is why the last conflict copy the hub had to keep did not fit
	// on the daemon host ("" since one did): the decision waits.
	hostFull atomic.Pointer[string]
}

// OpenHub opens the hub of the roots named (".claude", ".mnt", ...) with its
// state in dir (~/.fleet/sync/<fleet>, see FleetDir). The zero Table is
// syncpolicy.Defaults.
func OpenHub(dir string, table syncpolicy.Table, roots []string, opts HubOptions) (*Hub, error) {
	if len(table.Roots()) == 0 {
		table = syncpolicy.Defaults()
	}
	if opts.Policies == nil {
		opts.Policies = DefaultPolicies()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NormalDelay == 0 {
		opts.NormalDelay = 100 * time.Millisecond
	}
	if opts.BulkDelay == 0 {
		opts.BulkDelay = time.Second
	}
	if opts.PauseRetry == 0 {
		opts.PauseRetry = 10 * time.Second
	}
	if opts.TombstoneTTL == 0 {
		opts.TombstoneTTL = 24 * time.Hour
	}
	if opts.MaxConflicts == 0 {
		opts.MaxConflicts = 200
	}
	if opts.MaxBatch == 0 {
		opts.MaxBatch = 256
	}
	if opts.MinFree == 0 {
		opts.MinFree = 0.05
	}
	if opts.Space == nil {
		opts.Space = diskSpace
	}
	h := &Hub{
		dir: dir, table: table, policies: opts.Policies, opts: opts, log: opts.Logger,
		conflicts: &conflictStore{dir: filepath.Join(dir, "conflicts"), max: opts.MaxConflicts},
		roots:     map[string]*hubRoot{},
		sides:     map[string]*hubSide{},
	}
	for _, name := range roots {
		if h.roots[name] != nil {
			h.Close()
			return nil, fmt.Errorf("sharesync: root %s given twice", name)
		}
		root, err := h.openHubRoot(name)
		if err != nil {
			h.Close()
			return nil, err
		}
		h.roots[name] = root
		h.order = append(h.order, root)
	}
	return h, nil
}

func (h *Hub) now() time.Time {
	return h.opts.Now()
}

// Attach adds a side, which holds what hello says, and starts sending it
// what it lacks. A side holding revs of another epoch (the hub's state is
// new to it) has each path it holds taken as a change the hub has not seen,
// so nothing it holds is replaced unseen. The side must take the Welcome
// (Tree.Welcome) before its next report. Attaching an ID again replaces the
// side.
func (h *Hub) Attach(ctx context.Context, info SideInfo, peer Peer, hello Hello) (Welcome, error) {
	if info.ID == "" || peer == nil {
		return Welcome{}, errors.New("sharesync: a side needs an id and a peer")
	}
	h.Detach(info.ID)
	s := &hubSide{info: info, peer: peer, wake: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return Welcome{}, ErrClosed
	}
	h.sides[info.ID] = s
	h.mu.Unlock()

	hellos := map[string]RootHello{}
	for _, rh := range hello.Roots {
		hellos[rh.Root] = rh
	}
	type unseen struct {
		root *hubRoot
		held Held
	}
	var stale []unseen
	w := Welcome{Epochs: map[string]string{}}
	now := h.now()
	for _, root := range h.order {
		rh := hellos[root.name]
		root.mu.Lock()
		sr := newSideRoot(s)
		if info.LoginsOnly {
			sr.only = root.locationScoped
		}
		root.sides[info.ID] = sr
		w.Epochs[root.name] = root.epoch
		if root.broken != nil {
			// Not synced (a newer fleet's state): nothing to take or send.
			root.mu.Unlock()
			continue
		}
		matching := rh.Epoch != "" && rh.Epoch == root.epoch
		regressed := false
		for _, held := range rh.Held {
			if !validPath(held.Path) || held.Entry.check() != nil || !held.Entry.Live() || !sr.keeps(held.Path) {
				continue
			}
			var cur *hubEntry
			if k, _, ok := root.keyFor(held.Path, info.logins()); ok {
				cur = root.entries[k]
			}
			switch {
			case !matching:
			case held.Rev > root.rev:
				// The hub lost state the side had heard of (a crash before
				// a write reached the disk): never hand that rev out again.
				root.rev, regressed = held.Rev, true
			case cur != nil && cur.Rev == held.Rev && !cur.Entry.Same(held.Entry):
				// The same rev for other content: a rev given out again after
				// such a loss. Neither copy is taken on trust.
			default:
				sr.held[held.Path] = held.Rev
				continue
			}
			sr.held[held.Path] = 0
			stale = append(stale, unseen{root, held})
		}
		if regressed {
			h.log.Warn("sharesync: a side holds revs the hub does not", "root", root.name, "side", info.ID)
			if err := root.putMeta(); err != nil {
				h.log.Error("sharesync: recording state", "root", root.name, "err", err)
			}
		}
		for k, e := range root.entries {
			if k.loc != "" && k.loc != info.logins() {
				continue
			}
			if rev, ok := sr.held[k.path]; e.Entry.Live() && (!ok || rev != e.Rev) {
				sr.mark(k.path, now)
			}
		}
		for p := range sr.held {
			sr.mark(p, now)
		}
		root.mu.Unlock()
	}
	for _, u := range stale {
		h.process(ctx, u.root, s, Change{Path: u.held.Path, Entry: u.held.Entry}, true)
		u.root.mu.Lock()
		if sr := u.root.sides[info.ID]; sr != nil {
			// Held at a rev the hub cannot vouch for: an op tells it the rev.
			sr.held[u.held.Path] = 0
			sr.mark(u.held.Path, h.now())
		}
		u.root.mu.Unlock()
	}
	s.started.Add(1)
	go h.run(s)
	return w, nil
}

// Detach removes a side and stops sending to it.
func (h *Hub) Detach(id string) {
	h.mu.Lock()
	s := h.sides[id]
	delete(h.sides, id)
	h.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	s.started.Wait()
	for _, root := range h.order {
		root.mu.Lock()
		delete(root.sides, id)
		for hk := range root.holds {
			if hk.side == id {
				delete(root.holds, hk)
			}
		}
		root.mu.Unlock()
	}
}

// side returns an attached side.
func (h *Hub) side(id string) *hubSide {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sides[id]
}

// sideList returns the attached sides.
func (h *Hub) sideList() []*hubSide {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*hubSide, 0, len(h.sides))
	for _, s := range h.sides {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *hubSide) int { return strings.Compare(a.info.ID, b.info.ID) })
	return out
}

// Report takes a side's changes in one root and answers each. The hub's
// state for every accepted change is on disk before Report returns.
func (h *Hub) Report(ctx context.Context, side string, r Report) (Reply, error) {
	s := h.side(side)
	if s == nil {
		return Reply{}, fmt.Errorf("sharesync: report from %s: %w", side, ErrNotAttached)
	}
	root := h.roots[r.Root]
	changes := slices.Clone(r.Changes)
	sortChanges(changes)
	if root == nil {
		reply := Reply{}
		for _, c := range changes {
			reply.Acks = append(reply.Acks, Ack{Path: c.Path, Entry: c.Entry, Status: Ignored})
		}
		return reply, nil
	}
	root.mu.Lock()
	reply := Reply{Epoch: root.epoch}
	broken := root.broken
	root.mu.Unlock()
	stale := r.Epoch != reply.Epoch
	for _, c := range changes {
		if broken != nil {
			reply.Acks = append(reply.Acks, Ack{Path: c.Path, Entry: c.Entry, Status: Deferred})
			continue
		}
		reply.Acks = append(reply.Acks, h.process(ctx, root, s, c, stale))
	}
	return reply, nil
}

// openContent opens a state's file content from offset on: from the hub's
// own blobs, else from a side holding that rev (but except).
func (h *Hub) openContent(ctx context.Context, root *hubRoot, k key, e Entry, rev uint64, offset int64, except string) (io.ReadCloser, error) {
	if e.Kind != File {
		return nil, fmt.Errorf("%s is a %s: %w", k.path, e.Kind, ErrNoSource)
	}
	if root.blobs.has(e.Hash) {
		if rc, err := root.blobs.open(e.Hash, offset); err == nil {
			return rc, nil
		}
	}
	root.mu.Lock()
	var origin string
	if cur := root.entries[k]; cur != nil && cur.Rev == rev {
		origin = cur.Origin
	}
	var holders []*hubSide
	for id, sr := range root.sides {
		if id == except || sr.held[k.path] != rev || rev == 0 || k.loc != "" && sr.side.info.logins() != k.loc {
			continue
		}
		holders = append(holders, sr.side)
	}
	root.mu.Unlock()
	// The origin first (it made the content), then the rest by name.
	slices.SortFunc(holders, func(a, b *hubSide) int {
		switch {
		case a.info.ID == origin:
			return -1
		case b.info.ID == origin:
			return 1
		}
		return strings.Compare(a.info.ID, b.info.ID)
	})
	if len(holders) == 0 {
		return nil, fmt.Errorf("%s: %w", k.path, errNoHolder)
	}
	var errs []error
	for _, s := range holders {
		rc, err := s.peer.Fetch(ctx, root.name, k.path, e, offset)
		if err == nil {
			return rc, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", s.info.ID, err))
	}
	return nil, fmt.Errorf("%s: %w: %w", k.path, ErrNoSource, errors.Join(errs...))
}

// errNoHolder: no attached side holds the content, and the hub has no copy.
var errNoHolder = fmt.Errorf("%w: no side holds it", ErrNoSource)

// hostRoom checks the daemon host can keep size more bytes of conflict copy
// and still have MinFree of its disk free.
func (h *Hub) hostRoom(size int64) error {
	sp, err := h.opts.Space(h.dir)
	if err != nil || sp.Total <= 0 {
		h.hostFull.Store(nil)
		return nil
	}
	if sp.Free-size < int64(h.opts.MinFree*float64(sp.Total)) {
		reason := fmt.Sprintf("the daemon host's disk is under %d%% free", int(h.opts.MinFree*100+0.5))
		h.hostFull.Store(&reason)
		return fmt.Errorf("%w on the daemon host: %d bytes needed, %d free of %d", errNoSpace, size, sp.Free, sp.Total)
	}
	h.hostFull.Store(nil)
	return nil
}

// Conflicts returns the conflict copies kept, oldest first.
func (h *Hub) Conflicts() ([]Conflict, error) {
	return h.conflicts.list()
}

// Kick retries a side now: its pauses and backoffs end (the share was grown,
// the side is back).
func (h *Hub) Kick(side string) {
	s := h.side(side)
	if s == nil {
		return
	}
	for _, root := range h.order {
		root.mu.Lock()
		if sr := root.sides[side]; sr != nil {
			sr.paused, sr.retryAt = "", time.Time{}
			clear(sr.backoff)
		}
		root.mu.Unlock()
	}
	s.mu.Lock()
	s.downUntil, s.downN = time.Time{}, 0
	s.mu.Unlock()
	s.poke()
}

// Flush asks every side to rescan, then delivers everything pending without
// the lanes' delays, until every side holds every path's state. It returns a
// FlushError naming what is still behind when ctx ends first, or at once
// when all that is left cannot move (paused for space, over a cap,
// refused, or a side that could not rescan).
func (h *Hub) Flush(ctx context.Context) error {
	return h.flush(ctx, "", nil)
}

// FlushSide is Flush for what one side is owed: every side is asked to
// rescan (so a change made a moment ago anywhere is in), then side gets
// what it lacks without the lanes' delays, until it holds the state of every
// path want keeps (nil keeps every path), and no change of such a path waits
// for a root's cap. Only side's own rescan failing counts against it.
func (h *Hub) FlushSide(ctx context.Context, side string, want func(root, path string) bool) error {
	if h.side(side) == nil {
		return fmt.Errorf("sharesync: flushing %s: %w", side, ErrNotAttached)
	}
	return h.flush(ctx, side, want)
}

// flush is Flush, narrowed to one side's paths that want keeps when side is
// set.
func (h *Hub) flush(ctx context.Context, side string, want func(root, path string) bool) error {
	h.flushing.Add(1)
	defer h.flushing.Add(-1)
	var unreached []string
	down := map[string]bool{}
	for _, s := range h.sideList() {
		if err := s.peer.Rescan(ctx); err != nil {
			// What it is owed waits for it, and cannot move now.
			down[s.info.ID] = true
			if side == "" || s.info.ID == side {
				unreached = append(unreached, fmt.Sprintf("%s: rescan: %v", s.info.ID, err))
			} else {
				h.log.Debug("sharesync: a side could not rescan for another's flush", "side", s.info.ID, "for", side, "err", err)
			}
		}
		s.poke()
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		pending, stuck := h.pending(side, want, down)
		if len(pending) == 0 && len(unreached) == 0 {
			return nil
		}
		if stuck {
			return &FlushError{Pending: append(unreached, pending...)}
		}
		select {
		case <-ctx.Done():
			return &FlushError{Pending: append(unreached, pending...)}
		case <-tick.C:
		}
	}
}

// pending lists each side and root still behind (only side's paths want
// keeps when side is set); stuck is set when none of it can move (down are
// sides that could not be reached).
func (h *Hub) pending(side string, want func(root, path string) bool, down map[string]bool) (pending []string, stuck bool) {
	stuck = true
	wanted := func(root, p string) bool { return want == nil || want(root, p) }
	for _, root := range h.order {
		root.mu.Lock()
		if root.broken != nil {
			root.mu.Unlock()
			continue
		}
		held := 0
		for hk := range root.holds {
			if wanted(root.name, hk.path) {
				held++
			}
		}
		if held > 0 {
			pending = append(pending, fmt.Sprintf("%s: %d changes wait: %s", root.name, held, root.capReason()))
		}
		ids := make([]string, 0, len(root.sides))
		for id := range root.sides {
			if side == "" || id == side {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			sr := root.sides[id]
			n, movable := 0, false
			for p := range sr.dirty {
				if !wanted(root.name, p) {
					continue
				}
				op, ok := root.opFor(sr, p)
				if !ok {
					continue
				}
				if r, ok := sr.blocked[p]; ok && r.rev == op.Rev {
					continue
				}
				n++
				if sr.paused == "" || op.Entry.Kind != File {
					movable = true
				}
			}
			for p := range sr.inflight {
				if wanted(root.name, p) {
					n++
					movable = true
				}
			}
			if n == 0 {
				continue
			}
			if down[id] {
				movable = false
			}
			line := fmt.Sprintf("%s %s: %d to send", id, root.name, n)
			if sr.paused != "" {
				line += " (paused: " + sr.paused + ")"
			}
			pending = append(pending, line)
			if movable {
				stuck = false
			}
		}
		root.mu.Unlock()
	}
	return pending, stuck
}

// capReason says why a root holds changes back.
func (root *hubRoot) capReason() string {
	return fmt.Sprintf("%s is over its %s cap", root.name, sizeString(root.cap))
}

// Close detaches every side and closes the hub's state.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	ids := make([]string, 0, len(h.sides))
	for id := range h.sides {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.Detach(id)
	}
	var errs []error
	for _, root := range h.order {
		root.mu.Lock()
		if root.journal != nil {
			errs = append(errs, root.compact(), root.journal.close())
			root.journal = nil
			root.broken = ErrClosed
		}
		root.mu.Unlock()
	}
	return errors.Join(errs...)
}

// sizeString prints a byte count for people.
func sizeString(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.4g %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// newHash returns the content hash sharesync uses.
func newHash() hash.Hash {
	return sha256.New()
}

// verifyCloser is a verifier that closes what it reads.
type verifyCloser struct {
	*verifier
	c io.Closer
}

func (v *verifyCloser) Close() error {
	return v.c.Close()
}
