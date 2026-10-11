package sharesync

import (
	"context"
	"io"
	"log/slog"
	"path"
	"time"
)

// hint.go is how a side learns of changes its watcher cannot see: on a
// share mounted from several nodes, a write from another node reaches the
// folder without an inotify event here. The pods on the other nodes watch
// the share themselves and tell fleetd what changed (podwire's TagShareHint
// streams), and the hub's end of the spoke's connection passes each on as a
// hint (RemoteOptions.Hints), which the spoke's side scans soon.

// Hint is paths of a root (relative to its folder) that changed where this
// side does not watch; "." is all of it.
type Hint struct {
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
}

// hintPaths is the most paths of one hint a side takes: more is a scan of
// the whole root.
const hintPaths = 256

// Hint has the side scan paths of root soon: each one's folder is listed
// first (a network file system lists a file another node made at once, but
// may take a minute to open one by a name it looked up and missed), then
// they are scanned as a watcher's changes are. A root the side does not
// have, or a path that is not one below it, is ignored.
func (s *Side) Hint(root string, paths []string) {
	tr := s.tree.roots[root]
	if tr == nil {
		return
	}
	if len(paths) > hintPaths {
		paths = []string{"."}
	}
	var marked []string
	listed := map[string]bool{}
	for _, p := range paths {
		if p != "." && !validPath(p) {
			continue
		}
		marked = append(marked, p)
		dir := "."
		if p != "." {
			dir = path.Dir(p)
		}
		if !listed[dir] {
			listed[dir] = true
			_, _ = tr.fs.ReadDir(dir)
		}
	}
	if len(marked) > 0 {
		s.mark(root, marked, false)
	}
}

// Watcher tells which paths below a folder changed: inotify on Linux.
// The pod agents use it to hint a spoke on another node (Side.Hint).
type Watcher struct{ w watcher }

// Watch watches the folder dir and every folder below it that skip does
// not name (relative, slash-separated); errors.ErrUnsupported where the
// system has no watcher. log gets its warnings (nil discards them).
func Watch(dir string, skip func(rel string) bool, log *slog.Logger) (*Watcher, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if skip == nil {
		skip = func(string) bool { return false }
	}
	w, err := newWatcher(dir, skip, log)
	if err != nil {
		return nil, err
	}
	return &Watcher{w: w}, nil
}

// Ready is signalled when changed paths are waiting.
func (w *Watcher) Ready() <-chan struct{} { return w.w.ready() }

// Drain returns the paths changed since the last drain, relative to the
// folder; "." is all of it (events were lost).
func (w *Watcher) Drain() []string { return w.w.drain() }

// Limited reports that the system's watch limit kept some folders
// unwatched.
func (w *Watcher) Limited() bool { return w.w.limited() }

// Close stops the watcher.
func (w *Watcher) Close() error { return w.w.close() }

// forwardHints sends hints to the spoke until ctx ends or hints closes: each
// is one message, never waited for (a hint lost costs a scan the periodic
// rescan does anyway).
func (r *remoteSide) forwardHints(ctx context.Context, hints <-chan Hint) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.w.done:
			return
		case h, ok := <-hints:
			if !ok {
				return
			}
			if len(h.Paths) > hintPaths {
				h.Paths = []string{"."}
			}
			if err := r.w.send(wireMsg{T: msgHint, Hint: &h}); err != nil {
				return
			}
		}
	}
}

// hintWait bounds how long a spoke waits to be attached before it drops a
// hint.
const hintWait = 30 * time.Second
