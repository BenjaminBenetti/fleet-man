package sharesync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

// TreeRoot is one synced folder of a side.
type TreeRoot struct {
	// Name is the root's path relative to the fleet's shared folder
	// (".claude", ".mnt", ".hostbind/1a2b3c4d"), as the hub names it. A
	// policy table root must cover it.
	Name string
	// Dir is the folder on this side. It must exist.
	Dir string
	// Reserved are paths beneath Dir, in the io/fs form, that fleet itself
	// prepares (the resolver's Layout paths below the root, and files a
	// daemon-side writer owns): the tree never removes, renames or replaces
	// one, or puts a symlink there (internal/safefs Options.Reserved).
	Reserved []string
	// Skip are paths beneath Dir the tree leaves alone: another root's
	// folder nested inside this one.
	Skip []string
	// Only, when set, are the names directly in Dir the root holds; the tree
	// leaves everything else there alone, as if not there. A bound file's
	// root is the folder it is in, holding only it (internal/mounts/hostbind).
	Only []string
	// ReadOnly: the side never writes Dir (a read-only host bind's folder on
	// the daemon host, which the hub takes changes from alone): an op of
	// anything but what the folder holds is answered with the folder's own
	// state (applyReadOnly), and the folder need not take the self-test's
	// writes.
	ReadOnly bool
}

// TreeOptions configure a Tree.
type TreeOptions struct {
	// StateDir holds the tree's manifests, one journal per root. It must be
	// outside every root's folder. Required.
	StateDir string
	// Table is the policy table; the zero Table means syncpolicy.Defaults.
	Table syncpolicy.Table
	// MinFree is the fraction of its disk a root keeps free: a write that
	// would leave less is refused as NoSpace. 0 refuses only a write that
	// does not fit. The daemon host's tree keeps 5%.
	MinFree float64
	// FullReason names this side's full disk in status ("share full").
	FullReason string
	// Space reports disk space; nil uses statfs(2).
	Space SpaceFunc
	// Settle is how long a file over BulkSize must go unmodified before a
	// scan reports it, so a file still being copied in is sent once. 0 is
	// 1s; negative is none.
	Settle time.Duration
	// MergeSettle is how long a settings or login file (a json3, toml3 or
	// credential path) must go unmodified before a scan reports it, so one
	// rewritten in place (Codex's auth.json) is never caught half written:
	// a TOML or YAML file cut short often still parses, and would be taken
	// for a change that removed what it lacks. Unmodified means by its
	// modification time, or, for a file whose clock runs ahead of the
	// scan's (an NFS server's), seen unchanged that long. 0 is 100ms;
	// negative is none.
	MergeSettle time.Duration
	// Logger gets the tree's warnings; nil discards them.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Tree is one side's synced folders: it scans them for changes, applies the
// hub's, and serves their content. A Tree is safe for concurrent use; each
// root is guarded on its own.
type Tree struct {
	opts  TreeOptions
	table syncpolicy.Table
	log   *slog.Logger
	roots map[string]*treeRoot
	names []string

	// use is held for reading by every operation and for writing by Close,
	// so a tree is never closed under one (the hub may still be fetching
	// from a side it just detached).
	use    sync.RWMutex
	closed bool

	// afterWrite, when set (tests), runs after an op's content is in place
	// and before the tree records it; an error stops there, as a crash
	// would.
	afterWrite func(root, path string) error
}

// treeRoot is one root of a Tree.
type treeRoot struct {
	TreeRoot
	tree   *Tree
	policy syncpolicy.Table
	fs     *safefs.Root
	// opened is the folder as it was opened, to tell when Dir is no
	// longer that folder.
	opened os.FileInfo

	mu      sync.Mutex
	epoch   string
	records map[string]*record
	journal *journal
	// failing are the paths scans cannot read, warned of once, each with
	// the scan that last found it so (scans counts them): only a scan that
	// started after that one may find the path readable again.
	failing map[string]uint64
	scans   uint64
	// settling are the merging files seen modified too recently, as first
	// seen (MergeSettle).
	settling map[string]settleMark
}

// settleMark is a file as a scan first saw it unsettled.
type settleMark struct {
	fp fingerprint
	at time.Time
}

// record is what a side last synced of one path.
type record struct {
	// Rev is the hub rev the side holds, or the base of a change not yet
	// seen (Sent). 0 is none known.
	Rev   uint64      `json:"r,omitempty"`
	Entry Entry       `json:"e"`
	FP    fingerprint `json:"f"`
	// Sent marks a local change the hub has not seen yet: Entry is the
	// side's own. The hub's ops never replace it until it has.
	Sent bool `json:"x,omitempty"`
	// Made marks a synced folder the side made for what an op put in it,
	// before the folder's own op came (mkdirParents): its mode is not the
	// side's to report, so the side holds it at no rev, and the folder's op
	// sets the mode when it comes.
	Made bool `json:"made,omitempty"`
}

// treeMeta is a tree root journal's metadata.
type treeMeta struct {
	Epoch string `json:"epoch,omitempty"`
	// Folder is the identity of the folder the records describe.
	Folder *fingerprint `json:"folder,omitempty"`
}

// OpenTree opens a side's roots. It refuses to start when safefs's self-test
// fails on a root's folder (the system does not give the guarantees the
// writes rely on), and logs whether the openat2 fast path is in use.
func OpenTree(roots []TreeRoot, opts TreeOptions) (*Tree, error) {
	if opts.StateDir == "" {
		return nil, errors.New("sharesync: tree needs a state directory")
	}
	t := &Tree{opts: opts, table: opts.Table, log: opts.Logger, roots: map[string]*treeRoot{}}
	if len(t.table.Roots()) == 0 {
		t.table = syncpolicy.Defaults()
	}
	if t.log == nil {
		t.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if t.opts.Now == nil {
		t.opts.Now = time.Now
	}
	if t.opts.Space == nil {
		t.opts.Space = diskSpace
	}
	if t.opts.Settle == 0 {
		t.opts.Settle = time.Second
	}
	if t.opts.MergeSettle == 0 {
		t.opts.MergeSettle = 100 * time.Millisecond
	}
	if t.opts.FullReason == "" {
		t.opts.FullReason = "disk full"
	}
	t.log.Info("sharesync tree", "fast_path", safefs.FastPath())
	for _, spec := range roots {
		tr, err := t.openRoot(spec)
		if err != nil {
			t.Close()
			return nil, err
		}
		t.roots[spec.Name] = tr
		t.names = append(t.names, spec.Name)
	}
	return t, nil
}

// openRoot opens one root and loads its manifest.
func (t *Tree) openRoot(spec TreeRoot) (*treeRoot, error) {
	if !validPath(spec.Name) {
		return nil, fmt.Errorf("sharesync: invalid root name %q", spec.Name)
	}
	if _, dup := t.roots[spec.Name]; dup {
		return nil, fmt.Errorf("sharesync: root %s given twice", spec.Name)
	}
	if _, _, ok := t.table.Root(spec.Name); !ok {
		return nil, fmt.Errorf("sharesync: no policy root covers %s", spec.Name)
	}
	if !spec.ReadOnly {
		if err := safefs.SelfTest(spec.Dir); err != nil {
			return nil, fmt.Errorf("sharesync: root %s (%s): %w", spec.Name, spec.Dir, err)
		}
	}
	sroot, err := safefs.Open(spec.Dir, safefs.Options{Reserved: spec.Reserved})
	if err != nil {
		return nil, fmt.Errorf("sharesync: root %s: %w", spec.Name, err)
	}
	opened, err := sroot.Lstat(".")
	if err != nil {
		sroot.Close()
		return nil, fmt.Errorf("sharesync: root %s: %w", spec.Name, err)
	}
	tr := &treeRoot{TreeRoot: spec, tree: t, policy: t.table, fs: sroot, opened: opened, records: map[string]*record{}, failing: map[string]uint64{}, settling: map[string]settleMark{}}
	j, raw, err := openJournal(filepath.Join(t.opts.StateDir, stateFile(spec.Name)))
	if err != nil {
		sroot.Close()
		return nil, fmt.Errorf("sharesync: root %s: %w", spec.Name, err)
	}
	tr.journal = j
	var meta treeMeta
	if val, ok := raw[metaKey]; ok {
		json.Unmarshal(val, &meta)
		tr.epoch = meta.Epoch
	}
	folder := fingerprintOf(opened)
	if meta.Folder != nil && (meta.Folder.Ino != folder.Ino || meta.Folder.Dev != folder.Dev) {
		// Another folder than the one the records describe (made again, or
		// restored elsewhere): taking them would read its missing entries as
		// deletes. The side starts over, holding nothing the hub must trust.
		t.log.Warn("sharesync: root folder replaced; its manifest is dropped", "root", spec.Name, "dir", spec.Dir)
		for key := range raw {
			if key != metaKey {
				j.del(key)
			}
		}
		clear(raw)
	}
	for key, val := range raw {
		if key == metaKey {
			continue
		}
		var rec record
		if json.Unmarshal(val, &rec) != nil || !validPath(key) || rec.Entry.check() != nil {
			t.log.Warn("sharesync: dropping an unreadable manifest record", "root", spec.Name, "path", key)
			j.del(key)
			continue
		}
		tr.records[key] = &rec
	}
	// After the records are in: the commit may compact the journal from them.
	if err := tr.putMeta(); err != nil {
		j.close()
		sroot.Close()
		return nil, fmt.Errorf("sharesync: root %s: %w", spec.Name, err)
	}
	return tr, nil
}

// stateFile is the journal file name of a root.
func stateFile(name string) string {
	return "root-" + strings.NewReplacer("%", "%25", "/", "%2F").Replace(name) + ".json"
}

// Roots returns the tree's root names in the order given.
func (t *Tree) Roots() []string {
	return slices.Clone(t.names)
}

// enter starts an operation on the tree, or fails once it is closed; the
// caller calls t.use.RUnlock when done.
func (t *Tree) enter() error {
	t.use.RLock()
	if t.closed {
		t.use.RUnlock()
		return ErrClosed
	}
	return nil
}

// Close compacts and closes the tree's manifests and releases its folders,
// once the operations under way are done.
func (t *Tree) Close() error {
	t.use.Lock()
	defer t.use.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	var errs []error
	for _, name := range t.names {
		tr := t.roots[name]
		tr.mu.Lock()
		if tr.journal != nil {
			errs = append(errs, tr.compact(), tr.journal.close())
			tr.journal = nil
		}
		tr.fs.Close()
		tr.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Hello returns what the tree holds, for attaching to the hub: every path it
// holds at a rev the hub has seen. Local changes not yet seen are reported
// by the next scan instead.
func (t *Tree) Hello() Hello {
	var hello Hello
	if t.enter() != nil {
		return hello
	}
	defer t.use.RUnlock()
	for _, name := range t.names {
		tr := t.roots[name]
		tr.mu.Lock()
		rh := RootHello{Root: name, Epoch: tr.epoch, Held: []Held{}}
		for p, rec := range tr.records {
			if !rec.Sent && !rec.Made && rec.Entry.Live() {
				rh.Held = append(rh.Held, Held{Path: p, Rev: rec.Rev, Entry: rec.Entry})
			}
		}
		tr.mu.Unlock()
		slices.SortFunc(rh.Held, func(a, b Held) int { return strings.Compare(a.Path, b.Path) })
		hello.Roots = append(hello.Roots, rh)
	}
	return hello
}

// Welcome takes the hub's answer to Hello: a root whose epoch changed (the
// hub's state is new) holds no rev the hub knows, and the hub has taken
// what it holds as changes, so its revs are cleared for the hub to set.
func (t *Tree) Welcome(w Welcome) error {
	if err := t.enter(); err != nil {
		return err
	}
	defer t.use.RUnlock()
	for root, epoch := range w.Epochs {
		tr := t.roots[root]
		if tr == nil {
			continue
		}
		tr.mu.Lock()
		err := tr.adopt(epoch, false)
		tr.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// adopt moves the root to epoch, clearing every rev. unseen marks what the
// side holds as changes the hub has not seen (the epoch arrived with an op
// or an answer, not after a Hello the hub took them from).
func (tr *treeRoot) adopt(epoch string, unseen bool) error {
	if epoch == "" || epoch == tr.epoch {
		return nil
	}
	tr.epoch = epoch
	for p, rec := range tr.records {
		// A fresh record, so an op that checked the old one sees the change.
		fresh := *rec
		fresh.Rev = 0
		if unseen && fresh.Entry.Live() {
			fresh.Sent = true
		}
		if err := tr.putRecord(p, &fresh); err != nil {
			return err
		}
	}
	return tr.putMeta()
}

// putMeta journals the root's epoch and folder.
func (tr *treeRoot) putMeta() error {
	folder := fingerprintOf(tr.opened)
	if err := tr.journal.put(metaKey, treeMeta{Epoch: tr.epoch, Folder: &folder}); err != nil {
		return err
	}
	return tr.commit()
}

// Acked takes the hub's answers to a report.
func (t *Tree) Acked(root string, reply Reply) error {
	if err := t.enter(); err != nil {
		return err
	}
	defer t.use.RUnlock()
	tr := t.roots[root]
	if tr == nil {
		return fmt.Errorf("sharesync: no root %s", root)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if err := tr.adopt(reply.Epoch, true); err != nil {
		return err
	}
	for _, ack := range reply.Acks {
		old := tr.records[ack.Path]
		if old == nil || !old.Sent {
			continue
		}
		rec := *old
		same := rec.Entry.Same(ack.Entry)
		switch ack.Status {
		case Accepted:
			// A change made since was made on the accepted content: its
			// base is the accepted rev.
			rec.Rev = ack.Rev
			if same {
				rec.Sent = false
				if !rec.Entry.Live() {
					tr.dropRecord(ack.Path)
					continue
				}
			}
		case Superseded:
			if !same {
				continue
			}
			// The hub has seen it: the op it sends may replace it.
			rec.Sent = false
			if ack.Rev == 0 {
				// Refused though made on the hub's current state: the side
				// holds no rev of the hub's there, and a change made on
				// what it holds is not one made on that rev.
				rec.Rev = 0
			}
		default:
			continue
		}
		if err := tr.putRecord(ack.Path, &rec); err != nil {
			return err
		}
	}
	return tr.commit()
}

// commit writes the root's journal batch, compacting it when it has grown.
func (tr *treeRoot) commit() error {
	if tr.journal == nil {
		return ErrClosed
	}
	if err := tr.journal.commit(); err != nil {
		return err
	}
	if tr.journal.due(len(tr.records)) {
		return tr.compact()
	}
	return nil
}

// compact folds the root's journal into a snapshot.
func (tr *treeRoot) compact() error {
	records := make(map[string]any, len(tr.records)+1)
	for p, rec := range tr.records {
		records[p] = rec
	}
	folder := fingerprintOf(tr.opened)
	records[metaKey] = treeMeta{Epoch: tr.epoch, Folder: &folder}
	return tr.journal.compact(records)
}

// putRecord records p's state, journaled.
func (tr *treeRoot) putRecord(p string, rec *record) error {
	tr.records[p] = rec
	return tr.journal.put(p, rec)
}

// dropRecord forgets p, journaled.
func (tr *treeRoot) dropRecord(p string) {
	if _, ok := tr.records[p]; ok {
		delete(tr.records, p)
		tr.journal.del(p)
	}
}

// ErrRootMoved: a root's folder is gone, or is not the folder the tree
// opened (deleted, or moved and made again). Nothing below it is taken for
// deleted; the tree must be opened again on the folder there now.
var ErrRootMoved = errors.New("the folder is not the one sync opened")

// stillThere checks the root's folder is the one the tree opened.
func (tr *treeRoot) stillThere() error {
	now, err := os.Stat(tr.Dir)
	if err != nil || !os.SameFile(now, tr.opened) {
		return fmt.Errorf("sharesync: root %s (%s): %w", tr.Name, tr.Dir, ErrRootMoved)
	}
	return nil
}

// settling returns the paths of a root a scan found too recently modified
// to report yet (MergeSettle).
func (t *Tree) settling(root string) []string {
	if t.enter() != nil {
		return nil
	}
	defer t.use.RUnlock()
	tr := t.roots[root]
	if tr == nil {
		return nil
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]string, 0, len(tr.settling))
	for p := range tr.settling {
		out = append(out, p)
	}
	return out
}

// merging reports whether name's policy merges (or vets) a file's content:
// json3, toml3, credential.
func (tr *treeRoot) merging(name string) bool {
	policy, _ := tr.policy.PolicyFor(path.Join(tr.Name, name))
	return policy == syncpolicy.JSON3 || policy == syncpolicy.TOML3 || policy == syncpolicy.Credential
}

// settled reports whether a changed file of a merging policy has gone
// unmodified for MergeSettle (any other file is settled). Called without
// tr.mu.
func (tr *treeRoot) settled(c candidate) bool {
	wait := tr.tree.opts.MergeSettle
	if wait <= 0 || !tr.merging(c.name) {
		return true
	}
	now := tr.tree.opts.Now()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if age := now.Sub(c.info.ModTime()); age >= wait {
		delete(tr.settling, c.name)
		return true
	}
	mark, ok := tr.settling[c.name]
	if ok && mark.fp == c.fp && now.Sub(mark.at) >= wait {
		delete(tr.settling, c.name)
		return true
	}
	if !ok || mark.fp != c.fp {
		tr.settling[c.name] = settleMark{fp: c.fp, at: now}
	}
	return false
}

// synced reports whether the tree syncs name (relative to the root): not a
// temp file, not in a skipped folder, among the root's Only names when it has
// them, not location-local.
func (tr *treeRoot) synced(name string) bool {
	if !validPath(name) {
		return false
	}
	for _, elem := range strings.Split(name, "/") {
		if safefs.IsTemp(elem) {
			return false
		}
	}
	for _, skip := range tr.Skip {
		if within(name, skip) {
			return false
		}
	}
	if len(tr.Only) > 0 {
		top, _, _ := strings.Cut(name, "/")
		if !slices.Contains(tr.Only, top) {
			return false
		}
	}
	policy, ok := tr.policy.PolicyFor(path.Join(tr.Name, name))
	return ok && policy != syncpolicy.LocationLocal
}
