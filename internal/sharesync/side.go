package sharesync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// SideOptions configure a Side. Zero values take the defaults.
type SideOptions struct {
	// Rescan is how often every root is scanned in full besides what its
	// watcher reports: 60s.
	Rescan time.Duration
	// Poll is how often a root without a watcher is scanned: 2s.
	Poll time.Duration
	// Debounce is how long the side waits after a change before scanning,
	// so a burst of writes is one scan: 100ms.
	Debounce time.Duration
	// Again is how soon a path a scan could not settle is scanned again:
	// 300ms.
	Again time.Duration
	// NoWatch polls every root instead of watching it.
	NoWatch bool
	// Urgent reports paths to scan at once, without the debounce; nil is
	// the tree's login files (LoginPaths).
	Urgent func(root, path string) bool
	// ReportSize is the most changes in one report: 512.
	ReportSize int
	// Logger gets the side's warnings; nil discards them.
	Logger *slog.Logger
}

// Side runs one side's Tree: it watches the tree's folders, scans what
// changed and reports it to the hub, and is the hub's Peer for the tree. The
// daemon host's side runs in fleetd beside the hub; a pod's spoke runs the
// same with a Reporter over its session.
type Side struct {
	info SideInfo
	tree *Tree
	rep  Reporter
	opts SideOptions
	log  *slog.Logger
	kick chan struct{}

	scanMu sync.Mutex

	mu     sync.Mutex
	dirty  map[string]map[string]bool
	urgent bool
	again  map[string]map[string]bool
}

// NewSide returns the side info names, running tree and reporting to rep.
func NewSide(info SideInfo, tree *Tree, rep Reporter, opts SideOptions) *Side {
	if opts.Rescan == 0 {
		opts.Rescan = time.Minute
	}
	if opts.Poll == 0 {
		opts.Poll = 2 * time.Second
	}
	if opts.Debounce == 0 {
		opts.Debounce = 100 * time.Millisecond
	}
	if opts.Again == 0 {
		opts.Again = 300 * time.Millisecond
	}
	if opts.ReportSize == 0 {
		opts.ReportSize = 512
	}
	if opts.Urgent == nil {
		opts.Urgent = LoginPaths(tree.table)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Side{
		info: info, tree: tree, rep: rep, opts: opts, log: log, kick: make(chan struct{}, 1),
		dirty: map[string]map[string]bool{}, again: map[string]map[string]bool{},
	}
}

// Info returns the side's name.
func (s *Side) Info() SideInfo {
	return s.info
}

// Attach attaches the side to a hub in this process.
func (s *Side) Attach(ctx context.Context, h *Hub) error {
	w, err := h.Attach(ctx, s.info, s, s.tree.Hello())
	if err != nil {
		return err
	}
	return s.tree.Welcome(w)
}

// Apply is Peer.Apply: the tree applies the batch, and a path that held a
// change of its own is scanned next.
func (s *Side) Apply(ctx context.Context, b Batch) ([]Result, error) {
	results, err := s.tree.Apply(ctx, b)
	for _, r := range results {
		if r.Status == Changed {
			s.mark(b.Root, []string{r.Path}, true)
		}
	}
	return results, err
}

// Fetch is Peer.Fetch.
func (s *Side) Fetch(ctx context.Context, root, path string, e Entry, offset int64) (io.ReadCloser, error) {
	return s.tree.Fetch(ctx, root, path, e, offset)
}

// Rescan is Peer.Rescan: every root scanned and reported now. A settings
// or login file written a moment ago is waited for once (MergeSettle), so
// a flush does not leave it out.
func (s *Side) Rescan(ctx context.Context) error {
	var errs []error
	for _, root := range s.tree.Roots() {
		errs = append(errs, s.scan(ctx, root, nil))
	}
	settling := map[string][]string{}
	for _, root := range s.tree.Roots() {
		if paths := s.tree.settling(root); len(paths) > 0 {
			settling[root] = paths
		}
	}
	if len(settling) == 0 {
		return errors.Join(errs...)
	}
	wait := time.NewTimer(s.tree.opts.MergeSettle)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return errors.Join(append(errs, ctx.Err())...)
	case <-wait.C:
	}
	for root, paths := range settling {
		errs = append(errs, s.scan(ctx, root, paths))
	}
	return errors.Join(errs...)
}

// mark queues paths of root for the next scan; soon skips the debounce.
func (s *Side) mark(root string, paths []string, soon bool) {
	s.mu.Lock()
	set := s.dirty[root]
	if set == nil {
		set = map[string]bool{}
		s.dirty[root] = set
	}
	for _, p := range paths {
		set[p] = true
		if s.opts.Urgent != nil && s.opts.Urgent(root, p) {
			soon = true
		}
	}
	s.urgent = s.urgent || soon
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// scan scans paths of root (all of it for none) and reports the changes.
func (s *Side) scan(ctx context.Context, root string, paths []string) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	rep, again, err := s.tree.Scan(ctx, root, paths)
	if err != nil {
		return err
	}
	if len(again) > 0 {
		s.mu.Lock()
		set := s.again[root]
		if set == nil {
			set = map[string]bool{}
			s.again[root] = set
		}
		for _, p := range again {
			set[p] = true
		}
		s.mu.Unlock()
	}
	for len(rep.Changes) > 0 {
		n := min(len(rep.Changes), s.opts.ReportSize)
		chunk := Report{Root: rep.Root, Epoch: rep.Epoch, Changes: rep.Changes[:n]}
		rep.Changes = rep.Changes[n:]
		reply, err := s.rep.Report(ctx, s.info.ID, chunk)
		if err != nil {
			return err
		}
		if err := s.tree.Acked(root, reply); err != nil {
			return err
		}
	}
	return nil
}

// Run watches and scans the side's roots until ctx ends: a full scan first,
// then what the watchers report (after the debounce), a full scan every
// Rescan, and every Poll for a root without a watcher.
func (s *Side) Run(ctx context.Context) error {
	polled := map[string]bool{}
	var watchers []watcher
	defer func() {
		for _, w := range watchers {
			w.close()
		}
	}()
	watched := map[string]watcher{}
	for _, name := range s.tree.Roots() {
		tr := s.tree.roots[name]
		if s.opts.NoWatch {
			polled[name] = true
			continue
		}
		w, err := newWatcher(tr.Dir, func(rel string) bool { return !tr.synced(rel) }, s.log)
		if err != nil {
			if !errors.Is(err, errors.ErrUnsupported) {
				s.log.Warn("sharesync: cannot watch; polling", "root", name, "err", err)
			}
			polled[name] = true
			continue
		}
		watchers = append(watchers, w)
		watched[name] = w
		go func(name string, w watcher) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-w.ready():
					s.mark(name, w.drain(), false)
				}
			}
		}(name, w)
	}
	if err := s.Rescan(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("sharesync: scan", "side", s.info.ID, "err", err)
	}
	full := time.NewTicker(s.opts.Rescan)
	defer full.Stop()
	poll := time.NewTicker(s.opts.Poll)
	defer poll.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	again := time.NewTicker(s.opts.Again)
	defer again.Stop()
	// armed: the debounce runs from a burst's first change, so a file
	// written without pause is still scanned every Debounce.
	armed := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-full.C:
			s.report(ctx, s.Rescan(ctx))
		case <-poll.C:
			for name, w := range watched {
				if w.limited() {
					polled[name] = true
				}
			}
			for name := range polled {
				s.report(ctx, s.scan(ctx, name, nil))
			}
		case <-again.C:
			s.mu.Lock()
			pending := s.again
			s.again = map[string]map[string]bool{}
			s.mu.Unlock()
			for root, set := range pending {
				s.mark(root, keys(set), true)
			}
		case <-s.kick:
			s.mu.Lock()
			soon := s.urgent
			s.mu.Unlock()
			if soon {
				s.scanDirty(ctx)
			} else if !armed {
				armed = true
				debounce.Reset(s.opts.Debounce)
			}
		case <-debounce.C:
			armed = false
			s.scanDirty(ctx)
		}
	}
}

// scanDirty scans what was marked.
func (s *Side) scanDirty(ctx context.Context) {
	s.mu.Lock()
	dirty := s.dirty
	s.dirty, s.urgent = map[string]map[string]bool{}, false
	s.mu.Unlock()
	for root, set := range dirty {
		s.report(ctx, s.scan(ctx, root, keys(set)))
	}
}

// report logs a scan's failure.
func (s *Side) report(ctx context.Context, err error) {
	if err != nil && ctx.Err() == nil {
		s.log.Warn("sharesync: scan", "side", s.info.ID, "err", err)
	}
}

// keys returns a set's members.
func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
