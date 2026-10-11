package sharesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// key names one path's state: loc is "" for a fleet-wide path and the
// location for a path of location scope.
type key struct {
	loc, path string
}

// journalKey is the key's journal record key.
func (k key) journalKey() string {
	return k.loc + "\x00" + k.path
}

// parseKey reverses journalKey.
func parseKey(s string) (key, bool) {
	loc, p, ok := strings.Cut(s, "\x00")
	return key{loc: loc, path: p}, ok && validPath(p)
}

// hubEntry is the hub's state of one path.
type hubEntry struct {
	Entry Entry  `json:"e"`
	Rev   uint64 `json:"r"`
	// Origin is the side whose change this is, or "" for content the hub
	// made (a merge).
	Origin string `json:"o,omitempty"`
	// Base is the base rev the origin reported, so the origin's next change
	// on the same base (made before it heard back) is a fast forward.
	Base uint64 `json:"b,omitempty"`
	// At is when the state was accepted, in Unix nanoseconds.
	At int64 `json:"a,omitempty"`
	// Hist is the path's earlier contents kept for its policy's merges,
	// oldest first.
	Hist []histRev `json:"hist,omitempty"`
}

// histRev is one earlier content of a path, kept in the blob store.
type histRev struct {
	Rev  uint64 `json:"r"`
	Hash string `json:"h"`
}

// hubMeta is a hub root journal's metadata.
type hubMeta struct {
	Epoch string `json:"epoch"`
	Rev   uint64 `json:"rev"`
	// Forgot is the newest rev of a delete the hub has forgotten.
	Forgot uint64 `json:"forgot,omitempty"`
}

// hubRoot is the hub's state of one root: the sequencer of its revs.
type hubRoot struct {
	hub      *Hub
	name     string
	cap      int64
	warnings []syncpolicy.SizeWarning
	blobs    *blobStore

	mu    sync.Mutex
	epoch string
	rev   uint64
	// forgot is the newest rev of a delete forgotten: the rev of the delete
	// a side still holding such a path is sent, which an op that predates
	// it can never undo.
	forgot   uint64
	entries  map[key]*hubEntry
	children map[string]int
	total    int64
	// holds are the changes waiting for the root to fall back under its
	// cap, by side and path.
	holds   map[holdKey]bool
	sides   map[string]*sideRoot
	journal *journal
	// broken is why the root is not synced (its state is a newer fleet's).
	broken error
}

// holdKey names a change waiting for the cap.
type holdKey struct {
	side, path string
}

// openHubRoot opens a root's state.
func (h *Hub) openHubRoot(name string) (*hubRoot, error) {
	tableRoot, _, ok := h.table.Root(name)
	if !ok || !validPath(name) {
		return nil, fmt.Errorf("sharesync: no policy root covers %s", name)
	}
	if err := h.policies.covers(h.table, name); err != nil {
		return nil, err
	}
	root := &hubRoot{
		hub: h, name: name, cap: tableRoot.Cap, warnings: tableRoot.Warnings,
		blobs:    &blobStore{dir: filepath.Join(h.dir, "blobs", strings.TrimSuffix(stateFile(name), ".json"))},
		entries:  map[key]*hubEntry{},
		children: map[string]int{},
		holds:    map[holdKey]bool{},
		sides:    map[string]*sideRoot{},
	}
	j, raw, err := openJournal(filepath.Join(h.dir, "hub", stateFile(name)))
	if errors.Is(err, ErrNewerState) {
		root.broken = err
		h.log.Error("sharesync: root not synced", "root", name, "err", err)
		return root, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sharesync: root %s: %w", name, err)
	}
	root.journal = j
	var meta hubMeta
	for k, val := range raw {
		if k == metaKey {
			json.Unmarshal(val, &meta)
			continue
		}
		pk, ok := parseKey(k)
		var e hubEntry
		if !ok || json.Unmarshal(val, &e) != nil || e.Entry.check() != nil {
			h.log.Warn("sharesync: dropping an unreadable state record", "root", name, "key", k)
			continue
		}
		root.entries[pk] = &e
		root.count(nil, &e, pk)
		root.rev = max(root.rev, e.Rev)
	}
	root.epoch, root.rev, root.forgot = meta.Epoch, max(root.rev, meta.Rev), meta.Forgot
	if root.epoch == "" {
		root.epoch = newEpoch()
		if err := root.putMeta(); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// count moves the folder and size tallies from old to e at k.
func (root *hubRoot) count(old, e *hubEntry, k key) {
	parent := path.Dir(k.path)
	if old != nil && old.Entry.Live() {
		root.children[parent]--
		if root.children[parent] <= 0 {
			delete(root.children, parent)
		}
		root.total -= old.Entry.Size
	}
	if e != nil && e.Entry.Live() {
		root.children[parent]++
		root.total += e.Entry.Size
	}
}

// putMeta journals the root's epoch and rev. Called with root.mu held.
func (root *hubRoot) putMeta() error {
	if err := root.journal.put(metaKey, hubMeta{Epoch: root.epoch, Rev: root.rev, Forgot: root.forgot}); err != nil {
		return err
	}
	return root.journal.commit()
}

// merger returns the policy implementation of p, or false when the hub does
// not sync p.
func (root *hubRoot) merger(p string) (Merger, bool) {
	policy, ok := root.hub.table.PolicyFor(path.Join(root.name, p))
	if !ok || policy == syncpolicy.LocationLocal {
		return nil, false
	}
	m := root.hub.policies[policy]
	return m, m != nil
}

// keyFor returns the key of p as a side of location loc sees it.
func (root *hubRoot) keyFor(p, loc string) (key, Merger, bool) {
	m, ok := root.merger(p)
	if !ok {
		return key{}, nil, false
	}
	if m.Scope() == ScopeLocation {
		return key{loc: loc, path: p}, m, true
	}
	return key{path: p}, m, true
}

// setEntry makes e the state at k with a new rev, journaled, and marks every
// side that should have it, except except, to be sent it. Called with
// root.mu held.
func (root *hubRoot) setEntry(k key, e Entry, origin string, base uint64, hist []histRev, except string) (*hubEntry, error) {
	old := root.entries[k]
	root.rev++
	ne := &hubEntry{Entry: e, Rev: root.rev, Origin: origin, Base: base, At: root.hub.now().UnixNano(), Hist: hist}
	if err := root.journal.put(k.journalKey(), ne); err != nil {
		root.rev--
		return nil, err
	}
	if err := root.journal.commit(); err != nil {
		root.rev--
		return nil, err
	}
	root.entries[k] = ne
	root.count(old, ne, k)
	root.markDirty(k, except)
	if root.journal.due(len(root.entries)) {
		if err := root.compact(); err != nil {
			root.hub.log.Warn("sharesync: compacting state", "root", root.name, "err", err)
		}
	}
	return ne, nil
}

// markDirty marks k to be sent to every side it is for, but except.
func (root *hubRoot) markDirty(k key, except string) {
	now := root.hub.now()
	for id, sr := range root.sides {
		if id == except || k.loc != "" && sr.side.info.Location != k.loc {
			continue
		}
		sr.mark(k.path, now)
	}
}

// plan is a reported change on its way through process.
type plan struct {
	k    key
	m    Merger
	side *hubSide
	c    Change
	base uint64
	// cur is the state the change met (a copy), or nil.
	cur  *hubEntry
	fast bool
	// shape is the tree-shape verdict at the time of planning.
	shape shapeCheck
}

// shapeCheck is what the tree's shape makes of a change: every live entry
// sits in a live folder, and a folder with live content is never replaced
// by a file nor deleted.
type shapeCheck struct {
	// keep refuses the change outright, keeping a conflict copy of it when
	// conflict is set.
	keep     bool
	conflict bool
	reason   string
	// resurrect are deleted folders above the change that come back with
	// it, with the mode each last had.
	resurrect []string
	modes     []fs.FileMode
}

// equal reports whether two verdicts would commit the same.
func (s shapeCheck) equal(o shapeCheck) bool {
	return s.keep == o.keep && s.conflict == o.conflict && strings.Join(s.resurrect, "\x00") == strings.Join(o.resurrect, "\x00")
}

// checkShape returns the shape verdict of the change c. Called with root.mu
// held.
func (root *hubRoot) checkShape(c Change, cur *hubEntry) shapeCheck {
	var s shapeCheck
	if !c.Entry.Live() {
		if cur != nil && cur.Entry.Kind == Dir && root.children[c.Path] > 0 {
			s.keep, s.reason = true, "the folder holds content the delete had not seen"
		}
		return s
	}
	if c.Entry.Kind != Dir && root.children[c.Path] > 0 {
		return shapeCheck{keep: true, conflict: true, reason: "a folder with content stands there"}
	}
	for _, a := range ancestors(c.Path) {
		e := root.entries[key{path: a}]
		switch {
		case e == nil || !e.Entry.Live():
			s.resurrect = append(s.resurrect, a)
			mode := fs.FileMode(0o755)
			if e != nil && e.Entry.Mode != 0 {
				mode = e.Entry.Mode
			}
			s.modes = append(s.modes, mode)
		case e.Entry.Kind != Dir:
			return shapeCheck{keep: true, conflict: true, reason: fmt.Sprintf("%s is a %s now", a, e.Entry.Kind)}
		}
	}
	return s
}

// liveSize is the size a file state adds to the root.
func liveSize(e *hubEntry) int64 {
	if e == nil || !e.Entry.Live() {
		return 0
	}
	return e.Entry.Size
}

// overCap reports whether a file of size replacing cur would take the root
// past its cap. Called with root.mu held.
func (root *hubRoot) overCap(size int64, cur *hubEntry) bool {
	delta := size - liveSize(cur)
	return root.cap > 0 && delta > 0 && root.total+delta > root.cap
}

// process takes one reported change: content the hub already has is
// acknowledged, anything else goes to the path's policy, and its decision is
// committed if the path did not change meanwhile (else it is planned again).
func (h *Hub) process(ctx context.Context, root *hubRoot, side *hubSide, c Change, stale bool) Ack {
	ack := Ack{Path: c.Path, Entry: c.Entry, Status: Ignored}
	if !validPath(c.Path) || c.Entry.check() != nil {
		h.log.Warn("sharesync: ignoring a malformed change", "root", root.name, "side", side.info.ID, "path", c.Path)
		return ack
	}
	k, m, ok := root.keyFor(c.Path, side.info.Location)
	if !ok {
		return ack
	}
	base := c.Base
	if stale {
		base = 0
	}
	for range 4 {
		p, done := root.prepare(k, m, side, c, base)
		if done != nil {
			return *done
		}
		res, extra, err := h.decide(ctx, root, p)
		if err != nil {
			h.log.Warn("sharesync: change deferred", "root", root.name, "side", side.info.ID, "path", c.Path, "err", err)
			ack.Status = Deferred
			return ack
		}
		answer, retry := root.commitPlan(p, res, extra)
		h.log.Debug("sharesync: change", "root", root.name, "side", side.info.ID, "path", c.Path, "kind", c.Entry.Kind,
			"base", c.Base, "fast", p.fast, "decision", res.Decision, "ack", answer.Status, "rev", answer.Rev, "retry", retry)
		if !retry && answer.Status == Deferred && extra.conflict != "" {
			h.conflicts.remove(extra.conflict)
		}
		if !retry {
			if res.Reason != "" && answer.Status != Accepted || res.Conflict {
				h.log.Info("sharesync: concurrent change", "root", root.name, "path", c.Path, "side", side.info.ID, "decision", res.Decision, "reason", res.Reason)
			}
			return answer
		}
		if extra.conflict != "" {
			h.conflicts.remove(extra.conflict)
		}
	}
	ack.Status = Deferred
	return ack
}

// prepare plans a change, or answers it at once. Called without root.mu.
func (root *hubRoot) prepare(k key, m Merger, side *hubSide, c Change, base uint64) (*plan, *Ack) {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.broken != nil {
		return nil, &Ack{Path: c.Path, Entry: c.Entry, Status: Deferred}
	}
	if sr := root.sides[side.info.ID]; sr != nil {
		// The side has spoken for the path: what was waiting on it can go.
		delete(sr.backoff, c.Path)
		sr.reports[c.Path]++
	}
	hk := holdKey{side: side.info.ID, path: c.Path}
	delete(root.holds, hk)
	cur := root.entries[k]
	switch {
	case cur != nil && cur.Entry.Same(c.Entry):
		root.held(side, c.Path, cur)
		root.hub.log.Debug("sharesync: change already held", "root", root.name, "side", side.info.ID, "path", c.Path, "rev", cur.Rev)
		return nil, &Ack{Path: c.Path, Entry: c.Entry, Status: Accepted, Rev: cur.Rev}
	case cur == nil && !c.Entry.Live():
		return nil, &Ack{Path: c.Path, Entry: c.Entry, Status: Accepted}
	}
	if c.Entry.Kind == File && root.overCap(c.Entry.Size, cur) {
		root.holds[hk] = true
		return nil, &Ack{Path: c.Path, Entry: c.Entry, Status: Deferred}
	}
	p := &plan{k: k, m: m, side: side, c: c, base: base}
	if cur != nil {
		cp := *cur
		p.cur = &cp
	}
	p.fast = cur == nil || base == cur.Rev || cur.Origin == side.info.ID && base == cur.Base
	p.shape = root.checkShape(c, cur)
	return p, nil
}

// held records that side holds cur's state at p. Called with root.mu held.
func (root *hubRoot) held(side *hubSide, p string, cur *hubEntry) {
	sr := root.sides[side.info.ID]
	if sr == nil {
		return
	}
	if cur.Entry.Live() {
		sr.held[p] = cur.Rev
	} else {
		delete(sr.held, p)
	}
}

// planExtra is what decide prepared for the commit.
type planExtra struct {
	// conflict is the conflict copy kept, to drop if the plan is retried.
	conflict string
	// merged is the merged file's entry.
	merged Entry
	// hist is the incoming content's hash, kept for the policy's history.
	hist string
}

// decide runs the policy and does the slow work its decision needs: the
// conflict copy, the merged content, the history blob. Called without
// root.mu.
func (h *Hub) decide(ctx context.Context, root *hubRoot, p *plan) (MergeResult, planExtra, error) {
	var extra planExtra
	in := &MergeInput{
		Root: root.name, Path: p.c.Path, Side: p.side.info, Fast: p.fast,
		Incoming: Version{Entry: p.c.Entry, Side: p.side.info.ID, open: func(ctx context.Context) (io.ReadCloser, error) {
			return p.side.peer.Fetch(ctx, root.name, p.c.Path, p.c.Entry, 0)
		}},
	}
	if p.cur != nil {
		cur := *p.cur
		in.Current = &Version{Entry: cur.Entry, Rev: cur.Rev, Side: cur.Origin, open: func(ctx context.Context) (io.ReadCloser, error) {
			return h.openContent(ctx, root, p.k, cur.Entry, cur.Rev, 0, "")
		}}
		if p.m.History() > 0 && p.base > 0 {
			in.base = func(context.Context) ([]byte, bool, error) {
				for _, hr := range cur.Hist {
					if hr.Rev == p.base {
						rc, err := root.blobs.open(hr.Hash, 0)
						if err != nil {
							return nil, false, nil
						}
						defer rc.Close()
						data, err := io.ReadAll(io.LimitReader(rc, MaxMergeSize+1))
						return data, err == nil, err
					}
				}
				return nil, false, nil
			}
		}
	}
	var res MergeResult
	if p.shape.keep {
		res = MergeResult{Decision: Keep, Conflict: p.shape.conflict, Reason: p.shape.reason}
	} else {
		var err error
		if res, err = p.m.Merge(ctx, in); err != nil {
			return res, extra, err
		}
	}
	switch res.Decision {
	case Accept, Keep:
	case Merged:
		hash, err := root.blobs.put(res.Content)
		if err != nil {
			return res, extra, err
		}
		extra.merged = Entry{
			Kind: File, Mode: fileMode(File, p.c.Entry.Mode), Size: int64(len(res.Content)), Hash: hash,
			MTime: max(p.c.Entry.MTime, h.now().UnixNano()),
		}
		if p.cur != nil {
			cur := p.cur.Entry
			extra.merged.MTime = max(extra.merged.MTime, cur.MTime)
			// A union that only added lines grew from the current content:
			// copies holding it take the new lines in place.
			if cur.Kind == File && cur.Size < extra.merged.Size && hashOf(res.Content[:cur.Size]) == cur.Hash {
				extra.merged = extra.merged.grownFrom(cur)
			}
		}
	default:
		return res, extra, fmt.Errorf("policy for %s decided %d", p.c.Path, res.Decision)
	}
	if res.Conflict {
		id, err := h.keepConflict(ctx, root, p, res)
		if err != nil {
			return res, extra, fmt.Errorf("keeping a conflict copy: %w", err)
		}
		extra.conflict = id
	}
	if res.Decision == Accept && p.m.History() > 0 && p.c.Entry.Kind == File && p.c.Entry.Size <= MaxMergeSize {
		data, err := in.Incoming.Content(ctx)
		if err != nil {
			return res, extra, err
		}
		if extra.hist, err = root.blobs.put(data); err != nil {
			return res, extra, err
		}
	}
	return res, extra, nil
}

// keepConflict keeps the losing version of a decision as a conflict copy.
func (h *Hub) keepConflict(ctx context.Context, root *hubRoot, p *plan, res MergeResult) (string, error) {
	loser, side := p.c.Entry, p.side.info.ID
	open := func() (io.ReadCloser, error) {
		return p.side.peer.Fetch(ctx, root.name, p.c.Path, p.c.Entry, 0)
	}
	if res.Decision == Accept {
		if p.cur == nil {
			return "", nil
		}
		cur := *p.cur
		loser, side = cur.Entry, cur.Origin
		open = func() (io.ReadCloser, error) {
			return h.openContent(ctx, root, p.k, cur.Entry, cur.Rev, 0, "")
		}
	}
	var content io.ReadCloser
	switch loser.Kind {
	case File:
		if err := h.hostRoom(loser.Size); err != nil {
			return "", err
		}
		// A holder that is away (restarting, cut off) still has the losing
		// version: the decision waits for it rather than lose it.
		rc, err := open()
		if err != nil {
			return "", err
		}
		content = &verifyCloser{verifier: &verifier{r: rc, h: newHash(), want: loser.Hash, size: loser.Size}, c: rc}
	case Symlink:
		content = io.NopCloser(strings.NewReader(loser.Target))
	default:
		return "", nil
	}
	defer content.Close()
	c, err := h.conflicts.save(Conflict{
		Time: h.now(), Root: root.name, Path: p.c.Path, Location: p.k.loc, Side: side, Entry: loser, Reason: res.Reason,
	}, content)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// commitPlan commits a decision, unless the path or the tree's shape around
// it changed since it was planned (retry is set: plan again). Called
// without root.mu.
func (root *hubRoot) commitPlan(p *plan, res MergeResult, extra planExtra) (Ack, bool) {
	root.mu.Lock()
	defer root.mu.Unlock()
	ack := Ack{Path: p.c.Path, Entry: p.c.Entry}
	if root.broken != nil {
		ack.Status = Deferred
		return ack, false
	}
	now := root.entries[p.k]
	if now == nil && p.cur != nil || now != nil && (p.cur == nil || now.Rev != p.cur.Rev) {
		return ack, true
	}
	if !root.checkShape(p.c, now).equal(p.shape) {
		return ack, true
	}
	sr := root.sides[p.side.info.ID]
	size := p.c.Entry.Size
	if res.Decision == Merged {
		size = extra.merged.Size
	}
	if res.Decision != Keep && (p.c.Entry.Kind == File || res.Decision == Merged) && root.overCap(size, now) {
		root.holds[holdKey{side: p.side.info.ID, path: p.c.Path}] = true
		ack.Status = Deferred
		return ack, false
	}
	switch res.Decision {
	case Keep:
		if sr != nil {
			if p.c.Entry.Live() {
				// The side holds content the hub refused: bring it back.
				sr.held[p.c.Path] = 0
			} else {
				delete(sr.held, p.c.Path)
			}
			sr.mark(p.c.Path, root.hub.now())
		}
		ack.Status = Superseded
		if now != nil {
			ack.Rev = now.Rev
		}
		return ack, false
	}
	if err := root.resurrect(p); err != nil {
		root.hub.log.Error("sharesync: recording state", "root", root.name, "err", err)
		ack.Status = Deferred
		return ack, false
	}
	entry, origin, except := p.c.Entry, p.side.info.ID, p.side.info.ID
	if res.Decision == Merged {
		entry, origin, except = extra.merged, "", ""
	}
	var hist []histRev
	if now != nil {
		hist = now.Hist
	}
	if extra.hist != "" {
		hist = append(append([]histRev(nil), hist...), histRev{Rev: root.rev + 1, Hash: extra.hist})
		if n := p.m.History(); len(hist) > n {
			hist = hist[len(hist)-n:]
		}
	}
	ne, err := root.setEntry(p.k, entry, origin, p.c.Base, hist, except)
	if err != nil {
		root.hub.log.Error("sharesync: recording state", "root", root.name, "err", err)
		ack.Status = Deferred
		return ack, false
	}
	ack.Rev = ne.Rev
	if res.Decision == Merged {
		if sr != nil {
			sr.held[p.c.Path] = 0
		}
		ack.Status = Superseded
		return ack, false
	}
	if sr != nil {
		root.held(p.side, p.c.Path, ne)
	}
	ack.Status = Accepted
	return ack, false
}

// resurrect brings back the deleted folders above an accepted change.
// Called with root.mu held.
func (root *hubRoot) resurrect(p *plan) error {
	for i, a := range p.shape.resurrect {
		mode := fileMode(Dir, p.shape.modes[i])
		if _, err := root.setEntry(key{path: a}, Entry{Kind: Dir, Mode: mode}, p.side.info.ID, 0, nil, ""); err != nil {
			return err
		}
	}
	return nil
}

// compact folds the root's journal into a snapshot, forgetting deletes every
// attached side has had for TombstoneTTL, and the blobs nothing names.
// Called with root.mu held.
func (root *hubRoot) compact() error {
	cutoff := root.hub.now().Add(-root.hub.opts.TombstoneTTL).UnixNano()
	records := make(map[string]any, len(root.entries)+1)
	keep := map[string]bool{}
	for k, e := range root.entries {
		if !e.Entry.Live() && e.At < cutoff && !root.anyHolds(k.path) {
			root.journal.del(k.journalKey())
			delete(root.entries, k)
			root.forgot = max(root.forgot, e.Rev)
			continue
		}
		records[k.journalKey()] = e
		if e.Origin == "" && e.Entry.Kind == File {
			keep[e.Entry.Hash] = true
		}
		for _, hr := range e.Hist {
			keep[hr.Hash] = true
		}
	}
	records[metaKey] = hubMeta{Epoch: root.epoch, Rev: root.rev, Forgot: root.forgot}
	if err := root.journal.compact(records); err != nil {
		return err
	}
	root.blobs.gc(keep, root.hub.now().Add(-time.Hour))
	return nil
}

// anyHolds reports whether an attached side holds p.
func (root *hubRoot) anyHolds(p string) bool {
	for _, sr := range root.sides {
		if _, ok := sr.held[p]; ok {
			return true
		}
	}
	return false
}
