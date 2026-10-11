//go:build linux

package podagent

import (
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// sharehint_linux.go is the main agent's watch of a fleet share mounted
// from several nodes (multi-node mode): the share's spoke runs in one of the
// fleet's pods, and inotify there misses writes made on other nodes, so
// every pod's agent watches the share's folders on its own node and hints
// what changed to fleetd (a TagShareHint stream, podwire/sharehint.go),
// which hands it to the spoke. While the pod runs the spoke itself, its own
// watch sees the changes and the agent hints nothing. One inotify instance
// watches the whole tree (an instance counts against the uid's limit, which
// every pod on the node shares).

// Hinting's pace: changes are gathered for hintDebounce, a refused stream
// is not tried again for hintRetry, and a folder that could not be watched
// is tried again after hintRetry.
var (
	hintDebounce = 200 * time.Millisecond
	hintRetry    = 30 * time.Second
)

// shareHinter is one running watch, for one DesiredState.ShareHint.
type shareHinter struct {
	key  string
	stop chan struct{}
	done chan struct{}
}

// setShareHint starts, replaces or stops the agent's watch to match h.
func (a *agent) setShareHint(h *podwire.ShareHint) {
	key := ""
	if h != nil && len(h.Roots) > 0 {
		raw, _ := json.Marshal(h)
		key = string(raw)
	}
	a.hintMu.Lock()
	defer a.hintMu.Unlock()
	if a.hinter != nil && a.hinter.key == key {
		return
	}
	if a.hinter != nil {
		close(a.hinter.stop)
		<-a.hinter.done
		a.hinter = nil
	}
	if key == "" {
		return
	}
	sh := &shareHinter{key: key, stop: make(chan struct{}), done: make(chan struct{})}
	a.hinter = sh
	go func() {
		defer close(sh.done)
		a.runShareHint(h.Roots, sh.stop)
	}()
}

// stopShareHint ends the agent's watch (the session ended).
func (a *agent) stopShareHint() { a.setShareHint(nil) }

// hintRoots maps a path below the share's roots folder to its root (the
// longest root name it is in) and its path in it.
type hintRoots []string

func newHintRoots(roots []podwire.ShareHintRoot) hintRoots {
	var names hintRoots
	for _, r := range roots {
		names = append(names, r.Name)
	}
	// Longest first: a nested root wins over the one it is in.
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	return names
}

// of is rel's root and its path there; ok false when rel is in none.
func (h hintRoots) of(rel string) (root, inner string, ok bool) {
	for _, r := range h {
		switch {
		case rel == r:
			return r, ".", true
		case strings.HasPrefix(rel, r+"/"):
			return r, rel[len(r)+1:], true
		}
	}
	return "", "", false
}

// skip reports a folder the watch leaves alone: one neither in a root nor
// above one.
func (h hintRoots) skip(rel string) bool {
	if _, _, ok := h.of(rel); ok {
		return false
	}
	for _, r := range h {
		if strings.HasPrefix(r, rel+"/") {
			return false
		}
	}
	return true
}

// runShareHint watches the share's roots folder and hints its changes until
// stop closes.
func (a *agent) runShareHint(roots []podwire.ShareHintRoot, stop <-chan struct{}) {
	names := newHintRoots(roots)
	dir := filepath.Join(a.opts.ShareDir, podshare.RootsDir)
	var w *sharesync.Watcher
	defer func() {
		if w != nil {
			_ = w.Close()
		}
	}()
	retry := time.NewTimer(0)
	defer retry.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	s := &hintSender{a: a}
	defer s.close()
	pending := map[string]map[string]bool{}
	armed, warnedLimit := false, false
	var ready <-chan struct{}
	for {
		select {
		case <-stop:
			return
		case <-a.ctx.Done():
			return
		case <-retry.C:
			var err error
			w, err = sharesync.Watch(dir, names.skip, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				a.log.Warn("cannot watch the fleet share for changes to hint", "dir", dir, "err", err.Error())
				retry.Reset(hintRetry)
				continue
			}
			ready = w.Ready()
		case <-ready:
			for _, rel := range w.Drain() {
				if rel == "." {
					for _, r := range names {
						addHint(pending, r, ".")
					}
					continue
				}
				if root, inner, ok := names.of(rel); ok {
					addHint(pending, root, inner)
				}
			}
			if w.Limited() && !warnedLimit {
				warnedLimit = true
				a.log.Warn("the share's folders are past the inotify watch limit: changes below some are not hinted (the spoke's full scan finds them)")
			}
			if !armed && len(pending) > 0 {
				armed = true
				debounce.Reset(hintDebounce)
			}
		case <-debounce.C:
			armed = false
			batch := pending
			pending = map[string]map[string]bool{}
			if a.spokes.Load() > 0 {
				continue // this pod runs the spoke: its own watch sees these
			}
			s.send(batch)
		}
	}
}

func addHint(pending map[string]map[string]bool, root, p string) {
	set := pending[root]
	if set == nil {
		set = map[string]bool{}
		pending[root] = set
	}
	set[p] = true
}

// hintSender is the share-hint stream to fleetd, opened on first use.
type hintSender struct {
	a     *agent
	mu    sync.Mutex
	conn  net.Conn
	until time.Time // refused: not tried again until then
}

// send writes one hint per root of batch; a stream that fails is closed
// (the next batch opens one), one fleetd refuses is not tried for a while.
func (s *hintSender) send(batch map[string]map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if time.Now().Before(s.until) {
			return
		}
		c, err := podwire.DialSock(s.a.sess, podwire.TagShareHint, podwire.SockOpen{})
		if err != nil {
			s.until = time.Now().Add(hintRetry)
			if !podwire.IsSockRefused(err, "") {
				s.a.log.Warn("could not open the share-hint stream", "err", err.Error())
			}
			return
		}
		s.conn = c
	}
	for _, root := range slices.Sorted(maps.Keys(batch)) {
		paths := slices.Sorted(maps.Keys(batch[root]))
		if slices.Contains(paths, ".") || len(paths) > podwire.MaxHintPaths {
			paths = []string{"."}
		}
		_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := podwire.WriteShareHint(s.conn, podwire.ShareHintMsg{Root: root, Paths: paths}); err != nil {
			_ = s.conn.Close()
			s.conn = nil
			return
		}
	}
}

func (s *hintSender) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}
