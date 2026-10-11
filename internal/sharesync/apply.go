package sharesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
)

// maxAppend bounds the new bytes an op appends in place; a longer growth
// rewrites the file.
const maxAppend = 8 << 20

// Apply applies a batch of the hub's ops and returns one result per op. An
// op never replaces content the hub has not seen: a path holding a local
// change (made or not yet reported) is left alone as Changed. A batch of
// another epoch than the root's moves the root to it first.
func (t *Tree) Apply(ctx context.Context, b Batch) ([]Result, error) {
	if err := t.enter(); err != nil {
		return nil, err
	}
	defer t.use.RUnlock()
	tr := t.roots[b.Root]
	results := make([]Result, 0, len(b.Ops))
	if tr == nil {
		for _, op := range b.Ops {
			results = append(results, Result{Path: op.Path, Rev: op.Rev, Status: Refused, Reason: "not a root of this side"})
		}
		return results, nil
	}
	if err := tr.stillThere(); err != nil {
		// Writes would land in a folder no one sees.
		return nil, err
	}
	tr.mu.Lock()
	err := tr.adopt(b.Epoch, true)
	tr.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, op := range b.Ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		status, reason := tr.apply(ctx, op, b.Content)
		if status == NoSpace && reason == "" {
			reason = t.opts.FullReason
		}
		results = append(results, Result{Path: op.Path, Rev: op.Rev, Status: status, Reason: reason})
	}
	return results, nil
}

// local is what a path holds on the side when an op meets it.
type local struct {
	rec    *record
	info   fs.FileInfo
	exists bool
}

// apply applies one op.
func (tr *treeRoot) apply(ctx context.Context, op Op, content ContentFunc) (ResultStatus, string) {
	if !tr.synced(op.Path) {
		return Refused, "not synced on this side"
	}
	if err := op.Entry.check(); err != nil {
		return Refused, err.Error()
	}
	tr.mu.Lock()
	if rec := tr.records[op.Path]; rec != nil && rec.Rev > op.Rev {
		// Sent before the side's own newer change was accepted (revs only
		// grow): applying it would undo that change.
		tr.mu.Unlock()
		return Stale, fmt.Sprintf("the side holds rev %d", rec.Rev)
	}
	at, err := tr.look(op.Path)
	if err != nil {
		tr.mu.Unlock()
		return classify(err)
	}
	if at.rec != nil && at.exists && at.rec.Entry.Same(op.Entry) {
		// Already there (the side's own change, or applied before).
		err := tr.recordOp(op, at.info)
		tr.mu.Unlock()
		return classify(err)
	}
	if !at.exists && !op.Entry.Live() {
		tr.dropRecord(op.Path)
		err := tr.commit()
		tr.mu.Unlock()
		return classify(err)
	}
	if op.Entry.Kind != File {
		// Folders, symlinks and deletes are single system calls: done
		// under the lock, so no scan sees them half made.
		defer tr.mu.Unlock()
		switch op.Entry.Kind {
		case Gone:
			return tr.remove(op, at)
		case Dir:
			return classify(tr.makeDir(op, at))
		}
		return classify(tr.makeLink(op, at))
	}
	tr.mu.Unlock()
	return classify(tr.writeFile(ctx, op, at, content))
}

// look returns what p holds and checks it is what the tree last recorded,
// so applying an op there replaces nothing the hub has not seen. Called with
// tr.mu held.
func (tr *treeRoot) look(p string) (local, error) {
	at := local{rec: tr.records[p]}
	info, err := tr.fs.Lstat(p)
	switch {
	case err == nil:
		at.info, at.exists = info, true
	case !cannotExist(err):
		return at, err
	}
	return at, tr.unchanged(p, at)
}

// unchanged checks that what p holds is what its record says.
func (tr *treeRoot) unchanged(p string, at local) error {
	switch {
	case at.rec == nil:
		if at.exists {
			return localChange(p)
		}
	case at.rec.Sent:
		return localChange(p)
	case at.rec.Entry.Live():
		if !at.exists || fingerprintOf(at.info) != at.rec.FP {
			return localChange(p)
		}
	case at.exists:
		return localChange(p)
	}
	return nil
}

// still checks, with tr.mu held, that p holds what at saw.
func (tr *treeRoot) still(p string, at local) error {
	if tr.records[p] != at.rec {
		return localChange(p)
	}
	info, err := tr.fs.Lstat(p)
	switch {
	case cannotExist(err):
		if at.exists {
			return localChange(p)
		}
		return nil
	case err != nil:
		return err
	case !at.exists || fingerprintOf(info) != fingerprintOf(at.info):
		return localChange(p)
	}
	return nil
}

// recordOp records that p holds op's state, as info describes it. Called
// with tr.mu held.
func (tr *treeRoot) recordOp(op Op, info fs.FileInfo) error {
	if !op.Entry.Live() {
		tr.dropRecord(op.Path)
		return tr.commit()
	}
	if err := tr.putRecord(op.Path, &record{Rev: op.Rev, Entry: op.Entry, FP: fingerprintOf(info)}); err != nil {
		return err
	}
	return tr.commit()
}

// recordNow Lstats op's path and records it.
func (tr *treeRoot) recordNow(op Op) error {
	if hook := tr.tree.afterWrite; hook != nil {
		if err := hook(tr.Name, op.Path); err != nil {
			return err
		}
	}
	if !op.Entry.Live() {
		return tr.recordOp(op, nil)
	}
	info, err := tr.fs.Lstat(op.Path)
	if err != nil {
		return err
	}
	return tr.recordOp(op, info)
}

// remove applies a delete.
func (tr *treeRoot) remove(op Op, at local) (ResultStatus, string) {
	if err := tr.clear(op.Path, at); err != nil {
		return classify(err)
	}
	return classify(tr.recordNow(op))
}

// clear removes what p holds, for a delete or an op of another kind: a file
// or symlink, or a folder whose synced entries are gone (their deletes come
// first). What is left in such a folder is never synced (location-local
// files, sockets) and goes with it, as it would from a folder docker
// instances share; a nested root's folder never does. Called with tr.mu
// held.
func (tr *treeRoot) clear(p string, at local) error {
	if !at.exists {
		return nil
	}
	err := tr.fs.Remove(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !at.info.IsDir() {
		return err
	}
	for _, skip := range tr.Skip {
		if within(skip, p) {
			return err
		}
	}
	var synced error
	tr.fs.WalkDir(p, func(name string, d fs.DirEntry, werr error) error {
		switch {
		case werr != nil:
			synced = werr
		case name == p || !tr.synced(name):
			if name != p && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		default:
			// A synced entry not deleted yet: its delete comes first (or
			// it is a local change, reported next).
			synced = err
		}
		return fs.SkipAll
	})
	if synced != nil {
		return synced
	}
	return tr.fs.RemoveAll(p)
}

// makeDir applies a folder.
func (tr *treeRoot) makeDir(op Op, at local) error {
	mode := fileMode(Dir, op.Entry.Mode)
	if at.exists && at.info.IsDir() {
		if fileMode(Dir, at.info.Mode()) != mode {
			if err := tr.fs.Chmod(op.Path, mode); err != nil {
				return err
			}
		}
		return tr.recordNow(op)
	}
	if err := tr.clear(op.Path, at); err != nil {
		return err
	}
	if err := tr.mkdirParents(op.Path); err != nil {
		return err
	}
	if err := tr.fs.Mkdir(op.Path, mode); err != nil {
		return err
	}
	// Mkdir is masked by the umask and drops the sticky bit.
	if err := tr.fs.Chmod(op.Path, mode); err != nil {
		return err
	}
	return tr.recordNow(op)
}

// makeLink applies a symlink.
func (tr *treeRoot) makeLink(op Op, at local) error {
	if at.exists && at.info.Mode()&fs.ModeSymlink != 0 {
		if target, err := tr.fs.Readlink(op.Path); err == nil && target == op.Entry.Target {
			return tr.recordNow(op)
		}
	}
	if err := tr.clear(op.Path, at); err != nil {
		return err
	}
	if err := tr.mkdirParents(op.Path); err != nil {
		return err
	}
	if err := tr.fs.Symlink(op.Entry.Target, op.Path); err != nil {
		return err
	}
	return tr.recordNow(op)
}

// mkdirParents creates p's missing parent folders. The hub sends a folder
// before what is in it, so a missing one is a folder this side does not
// sync (its own op sets its mode if it is synced).
func (tr *treeRoot) mkdirParents(p string) error {
	parent := path.Dir(p)
	if parent == "." {
		return nil
	}
	if info, err := tr.fs.Lstat(parent); err == nil && info.IsDir() {
		return nil
	}
	return tr.fs.MkdirAll(parent, 0o755)
}

// writeFile applies a file: the new bytes appended in place when the side
// holds a content the new one grew from (checked against the whole
// content's hash first), the whole file replaced through a temp file
// otherwise. Content is read without the lock; the lock is taken again for
// the precondition check and the record.
func (tr *treeRoot) writeFile(ctx context.Context, op Op, at local, content ContentFunc) error {
	if content == nil {
		return ErrNoSource
	}
	if at.exists && at.info.IsDir() {
		tr.mu.Lock()
		err := tr.still(op.Path, at)
		if err == nil {
			err = tr.clear(op.Path, at)
		}
		tr.mu.Unlock()
		if err != nil {
			return err
		}
		at = local{rec: at.rec}
	}
	if at.exists && at.rec != nil {
		if offset := op.Entry.extends(at.rec.Entry); offset > 0 && op.Entry.Size-offset <= maxAppend {
			err := tr.appendFile(ctx, op, at, offset, content)
			if !errors.Is(err, errNoAppend) {
				return err
			}
		}
	}
	if err := tr.room(op.Entry.Size); err != nil {
		return err
	}
	if !at.exists {
		tr.mu.Lock()
		err := tr.mkdirParents(op.Path)
		tr.mu.Unlock()
		if err != nil {
			return err
		}
	}
	rc, err := content(ctx, op, 0)
	if err != nil {
		return err
	}
	defer rc.Close()
	locked := false
	defer func() {
		if locked {
			tr.mu.Unlock()
		}
	}()
	src := &verifier{r: rc, h: sha256.New(), want: op.Entry.Hash, size: op.Entry.Size}
	_, err = tr.fs.ReplaceFile(op.Path, src, op.Entry.Mode, time.Unix(0, op.Entry.MTime), func(current fs.FileInfo) error {
		tr.mu.Lock()
		locked = true
		if tr.records[op.Path] != at.rec {
			return localChange(op.Path)
		}
		switch {
		case current == nil && !at.exists:
			return nil
		case current == nil, !at.exists, fingerprintOf(current) != fingerprintOf(at.info):
			return localChange(op.Path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tr.recordWritten(op)
}

// recordWritten records a file op just written. A local write that landed
// between the write and this Lstat (the size or time is not what was
// written) must not be taken for the op's content, so the record is left
// dirty: the next scan reads the file and reports what it holds. Called
// with tr.mu held.
func (tr *treeRoot) recordWritten(op Op) error {
	if hook := tr.tree.afterWrite; hook != nil {
		if err := hook(tr.Name, op.Path); err != nil {
			return err
		}
	}
	info, err := tr.fs.Lstat(op.Path)
	if err != nil {
		return err
	}
	fp := fingerprintOf(info)
	if info.Size() != op.Entry.Size || !ourTime(info.ModTime(), op.Entry.MTime) {
		fp = dirtyFingerprint
	}
	if err := tr.putRecord(op.Path, &record{Rev: op.Rev, Entry: op.Entry, FP: fp}); err != nil {
		return err
	}
	return tr.commit()
}

// dirtyFingerprint matches no file: its path is read again by the next scan.
var dirtyFingerprint = fingerprint{Size: -1}

// ourTime reports whether a file's modification time is the one sharesync
// set, as a filesystem that keeps less precision stores it (truncated, by
// under a second); a write since would have set the time it was made.
func ourTime(stored time.Time, set int64) bool {
	d := set - stored.UnixNano()
	return d >= 0 && d < int64(time.Second)
}

// errNoAppend: the in-place append did not check out; rewrite the file.
var errNoAppend = errors.New("append does not apply")

// appendFile appends the bytes after offset to the side's file, which holds
// the content up to offset, once the whole result hashes as the op says.
func (tr *treeRoot) appendFile(ctx context.Context, op Op, at local, offset int64, content ContentFunc) error {
	if err := tr.room(op.Entry.Size - offset); err != nil {
		return err
	}
	rc, err := content(ctx, op, offset)
	if err != nil {
		return err
	}
	suffix, err := io.ReadAll(io.LimitReader(rc, op.Entry.Size-offset+1))
	rc.Close()
	if err != nil {
		return err
	}
	if int64(len(suffix)) != op.Entry.Size-offset {
		return errNoAppend
	}
	h := sha256.New()
	f, err := tr.fs.Open(op.Path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil && fingerprintOf(st) != fingerprintOf(at.info) {
		err = localChange(op.Path)
	}
	if err == nil {
		_, err = io.CopyN(h, f, offset)
	}
	f.Close()
	if err != nil {
		return err
	}
	h.Write(suffix)
	if hex.EncodeToString(h.Sum(nil)) != op.Entry.Hash {
		return errNoAppend
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if err := tr.still(op.Path, at); err != nil {
		return err
	}
	w, err := tr.fs.OpenFile(op.Path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	st, err = w.Stat()
	if err == nil && fingerprintOf(st) != fingerprintOf(at.info) {
		err = localChange(op.Path)
	}
	if err == nil {
		_, err = w.Write(suffix)
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if fileMode(File, at.info.Mode()) != op.Entry.Mode {
		if err := tr.fs.Chmod(op.Path, op.Entry.Mode); err != nil {
			return err
		}
	}
	mtime := time.Unix(0, op.Entry.MTime)
	if err := tr.fs.Chtimes(op.Path, mtime, mtime); err != nil {
		return err
	}
	return tr.recordWritten(op)
}

// room checks the root's disk can take need more bytes and keep MinFree of
// it free.
func (tr *treeRoot) room(need int64) error {
	if need <= 0 {
		return nil
	}
	sp, err := tr.tree.opts.Space(tr.Dir)
	if err != nil || sp.Total <= 0 {
		// Unknown: the write itself reports a full disk.
		return nil
	}
	keep := int64(tr.tree.opts.MinFree * float64(sp.Total))
	if sp.Free-need < keep {
		return fmt.Errorf("%w: %d bytes needed, %d free of %d", errNoSpace, need, sp.Free, sp.Total)
	}
	return nil
}

// verifier passes content through, failing with ErrStale at its end when it
// is not the size and hash expected, so a write of changed content never
// lands.
type verifier struct {
	r    io.Reader
	h    hash.Hash
	want string
	size int64
	n    int64
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	if v.n > v.size {
		return n, fmt.Errorf("%w: more than %d bytes", ErrStale, v.size)
	}
	if errors.Is(err, io.EOF) && (v.n != v.size || hex.EncodeToString(v.h.Sum(nil)) != v.want) {
		return n, fmt.Errorf("%w: %d bytes not hashing as sent", ErrStale, v.n)
	}
	return n, err
}

// Fetch opens the content of the file name in root, which the tree must hold
// as e (its own change not yet acknowledged included), from offset on. The
// read fails with ErrStale if the file is or becomes anything else.
func (t *Tree) Fetch(ctx context.Context, root, name string, e Entry, offset int64) (io.ReadCloser, error) {
	// Never waits on a tree being closed: the caller is often another
	// tree's Apply, which holds that tree open meanwhile.
	if !t.use.TryRLock() {
		return nil, ErrClosed
	}
	if t.closed {
		t.use.RUnlock()
		return nil, ErrClosed
	}
	// The reader holds its own open file: closing the tree leaves it be.
	defer t.use.RUnlock()
	tr := t.roots[root]
	if tr == nil {
		return nil, fmt.Errorf("sharesync: no root %s", root)
	}
	tr.mu.Lock()
	rec := tr.records[name]
	var fp fingerprint
	if rec != nil {
		fp = rec.FP
	}
	tr.mu.Unlock()
	if rec == nil || rec.Entry.Kind != File || e.Kind != File || rec.Entry.Hash != e.Hash || rec.Entry.Size != e.Size {
		return nil, fmt.Errorf("%s: %w", name, ErrStale)
	}
	if offset < 0 || offset > e.Size {
		return nil, fmt.Errorf("%s: offset %d of %d bytes: %w", name, offset, e.Size, fs.ErrInvalid)
	}
	f, err := tr.fs.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStale, err)
	}
	st, err := f.Stat()
	if err != nil || fingerprintOf(st) != fp {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrStale)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	fr := &fetchReader{f: f, fp: fp, left: e.Size - offset}
	if offset == 0 {
		fr.h, fr.want = sha256.New(), e.Hash
	}
	return fr, nil
}

// fetchReader reads a file a Tree serves and checks, at its end, that it was
// the file asked for throughout.
type fetchReader struct {
	f    *os.File
	fp   fingerprint
	left int64
	h    hash.Hash
	want string
}

func (r *fetchReader) Read(p []byte) (int, error) {
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.f.Read(p)
	r.left -= int64(n)
	if r.h != nil {
		r.h.Write(p[:n])
	}
	if r.left < 0 {
		return n, ErrStale
	}
	if errors.Is(err, io.EOF) {
		st, serr := r.f.Stat()
		switch {
		case r.left != 0, serr != nil, fingerprintOf(st) != r.fp:
			return n, ErrStale
		case r.h != nil && hex.EncodeToString(r.h.Sum(nil)) != r.want:
			return n, ErrStale
		}
	}
	return n, err
}

func (r *fetchReader) Close() error {
	return r.f.Close()
}
