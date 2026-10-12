package sharesync

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

// scanStep, when set, runs as a scan of the root folder dir starts its walk
// ("walk") and once it has walked ("walked"): a test seam, for what happens
// to the folder mid-scan.
var scanStep atomic.Pointer[func(dir, step string)]

func stepScan(dir, step string) {
	if f := scanStep.Load(); f != nil {
		(*f)(dir, step)
	}
}

// candidate is a path a scan found changed, before it is examined.
type candidate struct {
	name string
	info fs.FileInfo
	fp   fingerprint
	// prev is what the tree last recorded there.
	prev Entry
	// gone is set for a recorded path no longer there.
	gone bool
	// entry is the path's state, once examined.
	entry Entry
}

// Scan scans the root's folder, or only the given paths beneath it (each
// with everything below it), and returns what changed since the tree last
// synced as a Report for the hub, and the paths to scan again soon (a large
// file still being written, an entry that changed mid-scan). Changes the hub
// has not acknowledged yet are reported again. A folder that cannot be
// walked (a symlink swapped in, a permission error) never makes what was
// below it look deleted.
func (t *Tree) Scan(ctx context.Context, root string, paths []string) (Report, []string, error) {
	if err := t.enter(); err != nil {
		return Report{}, nil, err
	}
	defer t.use.RUnlock()
	tr := t.roots[root]
	if tr == nil {
		return Report{}, nil, fmt.Errorf("sharesync: no root %s", root)
	}
	if err := tr.stillThere(); err != nil {
		// A folder deleted whole (an rm -rf of ~/.fleet/workspaces/<fleet>/
		// .claude) is not a delete of everything in it to send everywhere.
		return Report{}, nil, err
	}
	stepScan(tr.Dir, "walk")
	areas := tr.reach(cover(paths))
	observed := map[string]fs.FileInfo{}
	var problems, again []string
	// failed are the paths that cannot be read for good (a folder or file
	// sync has no permission for): left to the periodic rescan, not scanned
	// again soon, and warned of once.
	failed := map[string]error{}
	for _, area := range areas {
		if area != "." && !tr.synced(area) {
			continue
		}
		tr.fs.WalkDir(area, func(name string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if err != nil {
				if name == area && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				problems = append(problems, name)
				if transient(err) {
					again = append(again, name)
				} else {
					failed[name] = err
				}
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if name == "." {
				return nil
			}
			if !tr.synced(name) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				problems, again = append(problems, name), append(again, name)
				return nil
			}
			if _, ok := kindOf(info); !ok {
				// A FIFO, socket or device is never synced, and never taken
				// for a delete of what was there.
				problems = append(problems, name)
				return nil
			}
			observed[name] = info
			return nil
		})
		if err := ctx.Err(); err != nil {
			// A walk cut short has not seen everything: what it missed is
			// not gone.
			return Report{}, nil, err
		}
	}
	stepScan(tr.Dir, "walked")

	tr.mu.Lock()
	var cands []candidate
	var changes []Change
	for name, info := range observed {
		fp := fingerprintOf(info)
		rec := tr.records[name]
		if rec != nil && rec.Entry.Live() && rec.FP == fp {
			if rec.Sent {
				changes = append(changes, Change{Path: name, Base: rec.Rev, Entry: rec.Entry})
			}
			continue
		}
		c := candidate{name: name, info: info, fp: fp}
		if rec != nil {
			c.prev = rec.Entry
		}
		cands = append(cands, c)
	}
	for name, rec := range tr.records {
		if observed[name] != nil || !inAny(name, areas) || inAny(name, problems) || !tr.synced(name) {
			continue
		}
		if !rec.Entry.Live() {
			if rec.Sent {
				changes = append(changes, Change{Path: name, Base: rec.Rev, Entry: rec.Entry})
			}
			continue
		}
		cands = append(cands, candidate{name: name, prev: rec.Entry, gone: true})
	}
	tr.mu.Unlock()

	examined := cands[:0]
	// looked are the files examined, whose settle (MergeSettle) the scan
	// has judged.
	looked := map[string]bool{}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return Report{}, nil, err
		}
		if !c.gone {
			looked[c.name] = true
			entry, err := tr.examine(c)
			switch {
			case err == nil:
				c.entry = entry
			case errors.Is(err, errUnsettled), transient(err):
				again = append(again, c.name)
				continue
			default:
				// A file sync cannot read (a root-owned 0600 file): not
				// synced, and not taken for gone either.
				failed[c.name] = err
				continue
			}
		}
		examined = append(examined, c)
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	// The folder may have gone, or been moved and made again, while the
	// walk read it: what the walk then found missing is not a delete to send
	// everywhere.
	if err := tr.stillThere(); err != nil {
		return Report{}, nil, err
	}
	for name, err := range failed {
		if !tr.failing[name] {
			t.log.Warn("sharesync: cannot scan", "root", root, "path", name, "err", err)
			tr.failing[name] = true
		}
	}
	for name := range tr.failing {
		if _, still := failed[name]; !still && inAny(name, areas) {
			delete(tr.failing, name)
		}
	}
	for name := range tr.settling {
		// One gone, or back to what was synced, is settling no more.
		if !looked[name] && inAny(name, areas) {
			delete(tr.settling, name)
		}
	}
	for _, c := range examined {
		// The path must still be what was examined: a write applied or made
		// since is the next scan's.
		now, err := tr.fs.Lstat(c.name)
		switch {
		case c.gone && cannotExist(err):
		case c.gone, err != nil, fingerprintOf(now) != c.fp:
			again = append(again, c.name)
			continue
		}
		rec := tr.records[c.name]
		if !c.gone && rec != nil && rec.Entry.Same(c.entry) {
			// The same content (touched, or rewritten with the same bytes).
			fresh := *rec
			fresh.FP = c.fp
			if err := tr.putRecord(c.name, &fresh); err != nil {
				return Report{}, nil, err
			}
			if fresh.Sent {
				changes = append(changes, Change{Path: c.name, Base: fresh.Rev, Entry: fresh.Entry})
			}
			continue
		}
		if c.gone && (rec == nil || !rec.Entry.Live()) {
			continue
		}
		fresh := &record{Entry: c.entry, FP: c.fp, Sent: true}
		if c.gone {
			fresh.Entry, fresh.FP = c.prev.tombstone(), fingerprint{}
		}
		if rec != nil {
			fresh.Rev = rec.Rev
		}
		if err := tr.putRecord(c.name, fresh); err != nil {
			return Report{}, nil, err
		}
		changes = append(changes, Change{Path: c.name, Base: fresh.Rev, Entry: fresh.Entry})
	}
	if err := tr.commit(); err != nil {
		return Report{}, nil, err
	}
	sortChanges(changes)
	for _, c := range changes {
		t.log.Debug("sharesync: tree reports", "root", root, "path", c.Path, "base", c.Base, "kind", c.Entry.Kind, "size", c.Entry.Size)
	}
	return Report{Root: root, Epoch: tr.epoch, Changes: changes}, again, nil
}

// examine returns the state of a changed path: errUnsettled when it must be
// looked at again soon (a large file modified within the settle time, or a
// path that changed while it was read), or why it cannot be read.
func (tr *treeRoot) examine(c candidate) (Entry, error) {
	kind, _ := kindOf(c.info)
	switch kind {
	case Dir:
		return Entry{Kind: Dir, Mode: fileMode(Dir, c.info.Mode())}, nil
	case Symlink:
		// The walk could list the folder, so a failure is the link being
		// replaced meanwhile.
		target, err := tr.fs.Readlink(c.name)
		if err != nil || target == "" {
			return Entry{}, errUnsettled
		}
		return Entry{Kind: Symlink, Target: target}, nil
	}
	settle := tr.tree.opts.Settle
	if c.info.Size() > BulkSize && settle > 0 && tr.tree.opts.Now().Sub(c.info.ModTime()) < settle {
		return Entry{}, errUnsettled
	}
	if !tr.settled(c) {
		return Entry{}, errUnsettled
	}
	f, err := tr.fs.Open(c.name)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || fingerprintOf(st) != c.fp {
		return Entry{}, errUnsettled
	}
	h := sha256.New()
	prefix := ""
	if c.prev.Kind == File && c.prev.Size > 0 && c.prev.Size < c.info.Size() {
		if _, err := io.CopyN(h, f, c.prev.Size); err != nil {
			return Entry{}, unsettledOr(err)
		}
		prefix = hex.EncodeToString(h.Sum(nil))
	}
	if _, err := io.Copy(h, f); err != nil {
		return Entry{}, unsettledOr(err)
	}
	if st, err := f.Stat(); err != nil || fingerprintOf(st) != c.fp {
		return Entry{}, errUnsettled
	}
	entry := Entry{
		Kind:  File,
		Mode:  fileMode(File, c.info.Mode()),
		Size:  c.info.Size(),
		MTime: c.info.ModTime().UnixNano(),
		Hash:  hex.EncodeToString(h.Sum(nil)),
	}
	if prefix != "" && prefix == c.prev.Hash {
		entry = entry.grownFrom(c.prev)
	}
	return entry, nil
}

// errUnsettled: a path changed while a scan read it, or is still being
// written; it is scanned again soon.
var errUnsettled = errors.New("changed while it was read")

// unsettledOr returns errUnsettled for a read cut short (the file shrank
// under it), else err.
func unsettledOr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errUnsettled
	}
	return err
}

// cannotExist reports whether an Lstat error says nothing is at the path in
// the synced tree: it is gone, or a folder above it is now a file or a
// symlink (which sync never follows).
func cannotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, safefs.ErrNotDir) ||
		errors.Is(err, safefs.ErrSymlink) || errors.Is(err, syscall.ENOTDIR)
}

// transient reports whether a scan error is a race the next scan resolves.
func transient(err error) bool {
	status, _ := classify(err)
	return status == Changed || errors.Is(err, fs.ErrNotExist)
}

// reach widens each area a scan cannot reach (it is gone, or a folder above
// it is now a file or a symlink) to its nearest ancestor that is there, so
// the change that made it unreachable is the scan's to find.
func (tr *treeRoot) reach(areas []string) []string {
	var out []string
	for _, area := range areas {
		for area != "." {
			if _, err := tr.fs.Lstat(area); err == nil || !cannotExist(err) {
				break
			}
			area = path.Dir(area)
		}
		out = append(out, area)
	}
	return cover(out)
}

// cover returns the folders a scan of paths walks: the shortest that cover
// them all, "." for everything (or for none given).
func cover(paths []string) []string {
	var out []string
	for _, p := range paths {
		p = path.Clean(p)
		if p == "." || p == "" {
			return []string{"."}
		}
		if validPath(p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"."}
	}
	slices.Sort(out)
	kept := out[:0]
	for _, p := range out {
		if len(kept) > 0 && within(p, kept[len(kept)-1]) {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// inAny reports whether name is one of folders or beneath one.
func inAny(name string, folders []string) bool {
	for _, folder := range folders {
		if within(name, folder) {
			return true
		}
	}
	return false
}

// sortChanges orders changes as the hub applies them: deletes deepest first,
// then everything else parents first.
func sortChanges(changes []Change) {
	slices.SortFunc(changes, func(a, b Change) int {
		ad, bd := !a.Entry.Live(), !b.Entry.Live()
		switch {
		case ad && !bd:
			return -1
		case !ad && bd:
			return 1
		case ad:
			if c := cmp.Compare(strings.Count(b.Path, "/"), strings.Count(a.Path, "/")); c != 0 {
				return c
			}
			return strings.Compare(b.Path, a.Path)
		}
		return strings.Compare(a.Path, b.Path)
	})
}

// sortOps orders ops as sortChanges orders changes.
func sortOps(ops []Op) {
	slices.SortFunc(ops, func(a, b Op) int {
		ad, bd := !a.Entry.Live(), !b.Entry.Live()
		switch {
		case ad && !bd:
			return -1
		case !ad && bd:
			return 1
		case ad:
			if c := cmp.Compare(strings.Count(b.Path, "/"), strings.Count(a.Path, "/")); c != 0 {
				return c
			}
			return strings.Compare(b.Path, a.Path)
		}
		return strings.Compare(a.Path, b.Path)
	})
}
