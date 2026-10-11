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
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

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
	areas := cover(paths)
	observed := map[string]fs.FileInfo{}
	var problems, again []string
	for _, area := range areas {
		if err := ctx.Err(); err != nil {
			return Report{}, nil, err
		}
		if area != "." && !tr.synced(area) {
			continue
		}
		tr.fs.WalkDir(area, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				if name == area && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				problems, again = append(problems, name), append(again, name)
				if !transient(err) {
					t.log.Warn("sharesync: cannot scan", "root", root, "path", name, "err", err)
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
	}

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
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return Report{}, nil, err
		}
		if !c.gone {
			entry, ok := tr.examine(c)
			if !ok {
				again = append(again, c.name)
				continue
			}
			c.entry = entry
		}
		examined = append(examined, c)
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
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
	return Report{Root: root, Epoch: tr.epoch, Changes: changes}, again, nil
}

// examine returns the state of a changed path. ok is false when it must be
// looked at again later: a large file modified within the settle time, or a
// path that changed while it was read.
func (tr *treeRoot) examine(c candidate) (Entry, bool) {
	kind, _ := kindOf(c.info)
	switch kind {
	case Dir:
		return Entry{Kind: Dir, Mode: fileMode(Dir, c.info.Mode())}, true
	case Symlink:
		target, err := tr.fs.Readlink(c.name)
		if err != nil || target == "" {
			return Entry{}, false
		}
		return Entry{Kind: Symlink, Target: target}, true
	}
	settle := tr.tree.opts.Settle
	if c.info.Size() > BulkSize && settle > 0 && tr.tree.opts.Now().Sub(c.info.ModTime()) < settle {
		return Entry{}, false
	}
	f, err := tr.fs.Open(c.name)
	if err != nil {
		return Entry{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || fingerprintOf(st) != c.fp {
		return Entry{}, false
	}
	h := sha256.New()
	prefix := ""
	if c.prev.Kind == File && c.prev.Size > 0 && c.prev.Size < c.info.Size() {
		if _, err := io.CopyN(h, f, c.prev.Size); err != nil {
			return Entry{}, false
		}
		prefix = hex.EncodeToString(h.Sum(nil))
	}
	if _, err := io.Copy(h, f); err != nil {
		return Entry{}, false
	}
	if st, err := f.Stat(); err != nil || fingerprintOf(st) != c.fp {
		return Entry{}, false
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
	return entry, true
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
