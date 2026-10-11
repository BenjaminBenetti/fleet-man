//go:build unix

package sharesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// testTable is the policy table the tests sync: a tool-like root with a
// mirror default, an append-union log and location-local paths; a root of
// user content; and a root with a small cap.
func testTable() syncpolicy.Table {
	return syncpolicy.NewTable(
		syncpolicy.Root{
			Path: "tool", Tool: "Tool", Default: syncpolicy.Mirror,
			Rules: []syncpolicy.Rule{
				{Pattern: "history.jsonl", Policy: syncpolicy.AppendUnion},
				{Pattern: "downloads/**", Policy: syncpolicy.LocationLocal},
				{Pattern: "**/*.lock", Policy: syncpolicy.LocationLocal},
			},
			Cap: 1 << 30,
		},
		syncpolicy.Root{Path: "user", Default: syncpolicy.Mirror},
		syncpolicy.Root{Path: "small", Default: syncpolicy.Mirror, Cap: 100},
	)
}

// testRoots are the roots of testTable.
var testRoots = []string{"tool", "user", "small"}

// rig is a hub and its sides, each side a folder per root under base.
type rig struct {
	t     *testing.T
	base  string
	table syncpolicy.Table
	roots []string
	hopts HubOptions
	topts func(id string) TreeOptions
	hub   *Hub
	// onOpen, when set, sees each side's tree before it attaches.
	onOpen func(s *rigSide)
	sides  map[string]*rigSide
	order  []string
	logs   *logBuffer
}

// rigSide is one side of a rig.
type rigSide struct {
	info SideInfo
	dir  string
	tree *Tree
	side *Side
	// peer is what the hub sees (the side, or a faulty wrapper).
	peer Peer
	// reserved are the root folders' reserved paths.
	reserved map[string][]string
	// stop ends the side's Run.
	stop func()
	// rep, when set, is the side's reporter instead of the hub.
	rep Reporter
}

// newRig opens a hub and the sides named (id=location).
func newRig(t *testing.T, sides ...string) *rig {
	t.Helper()
	r := &rig{t: t, base: t.TempDir(), table: testTable(), roots: testRoots, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.hopts = HubOptions{NormalDelay: time.Millisecond, BulkDelay: 2 * time.Millisecond, PauseRetry: 20 * time.Millisecond, Logger: r.logs.logger()}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1} }
	r.openHub()
	for _, spec := range sides {
		id, loc, _ := strings.Cut(spec, "=")
		if loc == "" {
			loc = id
		}
		r.addSide(SideInfo{ID: id, Location: loc})
	}
	t.Cleanup(r.close)
	return r
}

func (r *rig) openHub() {
	r.t.Helper()
	h, err := OpenHub(filepath.Join(r.base, "hub"), r.table, r.roots, r.hopts)
	if err != nil {
		r.t.Fatalf("OpenHub: %v", err)
	}
	r.hub = h
}

// addSide makes a side's folders and attaches it.
func (r *rig) addSide(info SideInfo) *rigSide {
	r.t.Helper()
	s := &rigSide{info: info, dir: filepath.Join(r.base, "sides", info.ID), reserved: map[string][]string{}}
	for _, root := range r.roots {
		if err := os.MkdirAll(filepath.Join(s.dir, root), 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	r.sides[info.ID] = s
	r.order = append(r.order, info.ID)
	r.openSide(s)
	return s
}

// openSide opens a side's tree and attaches it to the hub.
func (r *rig) openSide(s *rigSide) {
	r.t.Helper()
	var roots []TreeRoot
	for _, root := range r.roots {
		roots = append(roots, TreeRoot{Name: root, Dir: filepath.Join(s.dir, root), Reserved: s.reserved[root]})
	}
	opts := r.topts(s.info.ID)
	opts.StateDir = filepath.Join(r.base, "state", s.info.ID)
	opts.Table = r.table
	opts.Logger = r.logs.logger()
	tree, err := OpenTree(roots, opts)
	if err != nil {
		r.t.Fatalf("OpenTree(%s): %v", s.info.ID, err)
	}
	s.tree = tree
	if r.onOpen != nil {
		r.onOpen(s)
	}
	var rep Reporter = r.hub
	if s.rep != nil {
		rep = s.rep
	}
	s.side = NewSide(s.info, tree, rep, SideOptions{
		NoWatch: true, Poll: time.Hour, Rescan: time.Hour, Debounce: time.Millisecond, Again: 5 * time.Millisecond,
		Logger: r.logs.logger(),
	})
	switch p := s.peer.(type) {
	case nil, *Side:
		s.peer = s.side
	case *faultyPeer:
		p.inner = s.side
	case *countingPeer:
		p.inner = s.side
	}
	w, err := r.hub.Attach(context.Background(), s.info, s.peer, tree.Hello())
	if err != nil {
		r.t.Fatalf("Attach(%s): %v", s.info.ID, err)
	}
	if err := tree.Welcome(w); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.side.Run(ctx)
	}()
	s.stop = func() {
		cancel()
		<-done
	}
}

// restartSide closes and reopens a side, as a restarted spoke would.
func (r *rig) restartSide(id string) {
	r.t.Helper()
	s := r.sides[id]
	s.stop()
	r.hub.Detach(id)
	if err := s.tree.Close(); err != nil {
		r.t.Fatalf("closing %s: %v", id, err)
	}
	r.openSide(s)
}

// restartHub closes and reopens the hub, and every side with it.
func (r *rig) restartHub() {
	r.t.Helper()
	for _, id := range r.order {
		r.sides[id].stop()
	}
	if err := r.hub.Close(); err != nil {
		r.t.Fatalf("closing the hub: %v", err)
	}
	for _, id := range r.order {
		if err := r.sides[id].tree.Close(); err != nil {
			r.t.Fatal(err)
		}
	}
	r.openHub()
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
}

func (r *rig) close() {
	for _, s := range r.sides {
		s.stop()
	}
	r.hub.Close()
	for _, s := range r.sides {
		s.tree.Close()
	}
	if r.t.Failed() {
		r.t.Logf("log:\n%s", r.logs.String())
	}
}

// path returns a side's host path of root/rel.
func (r *rig) path(id, root, rel string) string {
	return filepath.Join(r.sides[id].dir, root, filepath.FromSlash(rel))
}

// write writes a file on a side, making its folders.
func (r *rig) write(id, root, rel, content string) {
	r.t.Helper()
	p := r.path(id, root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// read returns a side's file, or "<missing>".
func (r *rig) read(id, root, rel string) string {
	data, err := os.ReadFile(r.path(id, root, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return "<missing>"
	}
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

// sync rescans every side and delivers everything, failing on a stall.
func (r *rig) sync() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	if err := r.hub.Flush(ctx); err != nil {
		r.t.Fatalf("Flush: %v\n%s", err, r.dump())
	}
}

// flushErr flushes once and returns its error.
func (r *rig) flushErr(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return r.hub.Flush(ctx)
}

// snapshot is a folder tree: path to "f:<mode>:<content>", "d:<mode>" or
// "l:<target>".
type snapshot map[string]string

// tree returns a side's synced tree under root, location-local paths left
// out.
func (r *rig) tree(id, root string) snapshot {
	r.t.Helper()
	snap := snapshot{}
	dir := filepath.Join(r.sides[id].dir, root)
	tr := r.sides[id].tree.roots[root]
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			r.t.Fatalf("walking %s: %v", p, err)
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if !tr.synced(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, _ := d.Info()
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			snap[rel] = "l:" + target
		case d.IsDir():
			snap[rel] = fmt.Sprintf("d:%o", info.Mode()&(fs.ModePerm|fs.ModeSticky))
		case d.Type().IsRegular():
			data, _ := os.ReadFile(p)
			snap[rel] = fmt.Sprintf("f:%o:%s", info.Mode().Perm(), data)
		}
		return nil
	})
	return snap
}

// assertConverged checks every side holds the same tree in every root (the
// sides of one location the same login files too).
func (r *rig) assertConverged() {
	r.t.Helper()
	for _, root := range r.roots {
		want := r.tree(r.order[0], root)
		for _, id := range r.order[1:] {
			if got := r.tree(id, root); !equalSnap(got, want) {
				r.t.Errorf("root %s: %s differs from %s:\n%s", root, id, r.order[0], diffSnap(want, got))
			}
		}
	}
}

func equalSnap(a, b snapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func diffSnap(want, got snapshot) string {
	var lines []string
	for k, v := range want {
		if got[k] != v {
			lines = append(lines, fmt.Sprintf("  %s: want %q, got %q", k, v, got[k]))
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			lines = append(lines, fmt.Sprintf("  %s: unexpected %q", k, v))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// conflicts returns the conflict copies' contents.
func (r *rig) conflicts() []string {
	r.t.Helper()
	all, err := r.hub.Conflicts()
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, c := range all {
		data, err := os.ReadFile(c.File)
		if err != nil {
			r.t.Fatalf("conflict %s: %v", c.ID, err)
		}
		out = append(out, c.Root+"/"+c.Path+"="+string(data))
	}
	return out
}

// logBuffer collects a test's log.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// sum returns content's hash.
func sum(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// faultyPeer wraps a side, failing what its knobs say.
type faultyPeer struct {
	inner Peer
	mu    sync.Mutex
	// down fails every call as unreachable.
	down bool
	// lose applies a batch but loses its results.
	lose bool
	// fetchFail fails fetches.
	fetchFail bool
	// calls counts Apply calls.
	calls int
}

func (f *faultyPeer) Apply(ctx context.Context, b Batch) ([]Result, error) {
	f.mu.Lock()
	down, lose := f.down, f.lose
	f.calls++
	f.mu.Unlock()
	if down {
		return nil, errors.New("side unreachable")
	}
	results, err := f.inner.Apply(ctx, b)
	if lose {
		return nil, errors.New("connection dropped mid-batch")
	}
	return results, err
}

func (f *faultyPeer) Fetch(ctx context.Context, root, p string, e Entry, offset int64) (io.ReadCloser, error) {
	f.mu.Lock()
	down, fail := f.down, f.fetchFail
	f.mu.Unlock()
	if down || fail {
		return nil, errors.New("fetch failed")
	}
	return f.inner.Fetch(ctx, root, p, e, offset)
}

func (f *faultyPeer) Rescan(ctx context.Context) error {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return errors.New("side unreachable")
	}
	return f.inner.Rescan(ctx)
}

func (f *faultyPeer) set(fn func(f *faultyPeer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// countingPeer counts the content bytes the hub fetches from a side.
type countingPeer struct {
	inner Peer
	mu    sync.Mutex
	read  int64
}

func (c *countingPeer) Apply(ctx context.Context, b Batch) ([]Result, error) {
	return c.inner.Apply(ctx, b)
}

func (c *countingPeer) Fetch(ctx context.Context, root, p string, e Entry, offset int64) (io.ReadCloser, error) {
	rc, err := c.inner.Fetch(ctx, root, p, e, offset)
	if err != nil {
		return nil, err
	}
	return &countingReader{ReadCloser: rc, c: c}, nil
}

func (c *countingPeer) Rescan(ctx context.Context) error {
	return c.inner.Rescan(ctx)
}

type countingReader struct {
	io.ReadCloser
	c *countingPeer
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.c.mu.Lock()
	r.c.read += int64(n)
	r.c.mu.Unlock()
	return n, err
}

// dump describes what the hub still has to send, for a failed sync.
func (r *rig) dump() string {
	var b strings.Builder
	for _, root := range r.hub.order {
		root.mu.Lock()
		for id, sr := range root.sides {
			for p := range sr.dirty {
				op, ok := root.opFor(sr, p)
				if !ok {
					continue
				}
				k, _, _ := root.keyFor(p, sr.side.info.Location)
				e := root.entries[k]
				fmt.Fprintf(&b, "%s %s/%s: op rev %d %s held %v backoff %+v blocked %+v lastErr %q entry %+v\n",
					id, root.name, p, op.Rev, op.Entry.Kind, sr.held[p], sr.backoff[p], sr.blocked[p], sr.lastErr, e)
				if s := r.sides[id]; s != nil {
					tr := s.tree.roots[root.name]
					tr.mu.Lock()
					rec := tr.records[p]
					tr.mu.Unlock()
					info, err := os.Lstat(r.path(id, root.name, p))
					fmt.Fprintf(&b, "    side record %+v; disk %v %v\n", rec, info, err)
				}
			}
		}
		root.mu.Unlock()
	}
	return b.String()
}

// syncTimeout bounds a rig's sync.
var syncTimeout = 10 * time.Second
