//go:build linux

package podseed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// sync_linux.go is a clone's copier: it makes a tree on the clone's disk the
// same as the tree on the source's, again and again while the source runs,
// each pass writing only what changed since the one before (or since the
// tree was written at all), with the seed copier's fidelity (owners, modes,
// xattrs but security.selinux, hard links, holes, devices, times).
//
// A file is the same as what the last pass copied when its type, size, mode,
// owner and modification time are, and its change time is the one it had
// when that pass read it: a write, a chmod, a chown or a new xattr since all
// move the change time, even a write that put the old modification time
// back. A file that changed while it was copied keeps no change time, so the
// next pass copies it again.

// syncTempPrefix names the temp files a pass writes copies into before it
// renames them over their files.
const syncTempPrefix = ".fleet-clone-"

// syncer is one tree copy, across its passes.
type syncer struct {
	*copier
	src, dst string
	// live: the source runs. A vanished entry is skipped, not an error, and
	// a git dir's lock and temp files are not copied.
	live bool
	// exclude names paths, relative to dst, the copy leaves alone on both
	// sides.
	exclude func(rel string) bool

	seenMu sync.Mutex
	// seen is the change time each regular file had when a pass read it,
	// by its path relative to dst.
	seen map[string]unix.Timespec

	// changed counts what the current pass wrote or deleted.
	changed atomic.Int64
	tmpSeq  atomic.Int64

	// onList and onRead, when set, see each dir a pass lists and each file
	// it copies (by path relative to dst) as it does: tests of the order.
	onList, onRead func(rel string)
}

// gitPart is where in a git repository's own dir an entry is, for the order
// a live copy reads it in.
type gitPart uint8

const (
	gitNone gitPart = iota
	// gitOther is the rest of the repository's dir (hooks, info, lfs,
	// modules): copied as any file is.
	gitOther
	// gitMeta is what names objects (HEAD, the index, packed-refs, refs/,
	// logs/, worktrees/): a live copy reads it as the walk reaches it,
	// before objects/.
	gitMeta
	// gitObjects is objects/, read last.
	gitObjects
)

// gitChild is the part of an entry name (a dir when dir) of a repository's
// own dir.
func gitChild(name string, dir bool) gitPart {
	switch {
	case name == "objects":
		return gitObjects
	case !dir || name == "refs" || name == "logs" || name == "worktrees":
		return gitMeta
	}
	return gitOther
}

// newSyncer copies the tree at src onto dst.
func newSyncer(src, dst string, live bool, workers int, stats *copyStats) (*syncer, error) {
	c, err := newCopier(src, map[string]bool{}, map[string]bool{}, workers, stats)
	if err != nil {
		return nil, err
	}
	return &syncer{copier: c, src: src, dst: dst, live: live, seen: map[string]unix.Timespec{}, exclude: func(string) bool { return false }}, nil
}

// pass makes each of tops (top-level dirs of the tree) the same on dst as on
// src, deleting what src no longer has, and returns how many entries it wrote
// or deleted.
func (s *syncer) pass(ctx context.Context, tops []string) (int64, error) {
	s.changed.Store(0)
	s.mu.Lock()
	s.links = map[[2]uint64]string{}
	s.dirs = nil
	s.mu.Unlock()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan fileJob, 64)
	var wg sync.WaitGroup
	for range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := s.replaceFile(j.src, j.dst, &j.st); err != nil {
					cancel(err)
				}
			}
		}()
	}
	var err error
	for _, top := range tops {
		if err = s.syncTop(ctx, top, jobs); err != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		return s.changed.Load(), err
	}
	return s.changed.Load(), s.finishDirs()
}

// syncTop syncs one top-level dir; one the source lacks goes from dst too.
func (s *syncer) syncTop(ctx context.Context, top string, jobs chan<- fileJob) error {
	src, dst := filepath.Join(s.src, top), filepath.Join(s.dst, top)
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return s.remove(dst)
		}
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("the source disk's %s is not a directory", top)
	}
	return s.syncDir(ctx, src, dst, top, &st, gitNone, jobs)
}

// syncDir makes dst the dir src (stat st) is, rel its path relative to the
// tree's root; git is where it is in a git repository's own dir.
func (s *syncer) syncDir(ctx context.Context, src, dst, rel string, st *unix.Stat_t, git gitPart, jobs chan<- fileJob) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	var dstSt unix.Stat_t
	switch err := unix.Lstat(dst, &dstSt); {
	case err == nil && dstSt.Mode&unix.S_IFMT == unix.S_IFDIR:
	case err == nil:
		if err := s.remove(dst); err != nil {
			return err
		}
		fallthrough
	case errors.Is(err, unix.ENOENT):
		if err := unix.Mkdir(dst, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		s.changed.Add(1)
	default:
		return fmt.Errorf("stat %s: %w", dst, err)
	}
	s.mu.Lock()
	s.dirs = append(s.dirs, dirFinal{src: src, dst: dst, st: *st})
	s.mu.Unlock()
	if st.Dev != s.rootDev {
		return nil // another filesystem mounted in the source: never descended
	}

	if s.onList != nil {
		s.onList(rel)
	}
	names, err := readDirNames(src)
	if err != nil {
		if s.live && errors.Is(err, unix.ENOENT) {
			return nil
		}
		if s.singleUID && errors.Is(err, unix.EACCES) {
			s.warnf("%s: unreadable without root, left as it was", rel)
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	// A repository's own dir, here or nested in another's (a submodule's
	// under .git/modules).
	gitRoot := (git == gitNone || git == gitOther) && isGitDir(src, names)
	names = orderEntries(src, names, gitRoot)
	keep := map[string]bool{}
	for _, name := range names {
		crel := rel + "/" + name
		if s.exclude(crel) {
			keep[name] = true
			continue
		}
		if s.live && (gitRoot || git != gitNone) && skipLiveGit(name) {
			continue // a lock or temp file a running git holds: not the repo's
		}
		csrc, cdst := filepath.Join(src, name), filepath.Join(dst, name)
		var cst unix.Stat_t
		if err := unix.Lstat(csrc, &cst); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // gone since the listing
			}
			return fmt.Errorf("stat %s: %w", csrc, err)
		}
		keep[name] = true
		part := git
		if gitRoot {
			part = gitChild(name, cst.Mode&unix.S_IFMT == unix.S_IFDIR)
		}
		if err := s.syncEntry(ctx, csrc, cdst, crel, &cst, part, jobs); err != nil {
			return err
		}
	}
	// What the source no longer has goes.
	have, err := readDirNames(dst)
	if err != nil {
		return fmt.Errorf("read %s: %w", dst, err)
	}
	for _, name := range have {
		if strings.HasPrefix(name, syncTempPrefix) {
			continue // a copy a worker is writing
		}
		if !keep[name] && !s.exclude(rel+"/"+name) {
			if err := s.remove(filepath.Join(dst, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncEntry makes dst the entry src (stat st) is, git where it is in a git
// repository's own dir.
func (s *syncer) syncEntry(ctx context.Context, src, dst, rel string, st *unix.Stat_t, git gitPart, jobs chan<- fileJob) error {
	typ := st.Mode & unix.S_IFMT
	var dstSt unix.Stat_t
	dstErr := unix.Lstat(dst, &dstSt)
	if dstErr != nil && !errors.Is(dstErr, unix.ENOENT) {
		return fmt.Errorf("stat %s: %w", dst, dstErr)
	}
	exists := dstErr == nil
	if exists && dstSt.Mode&unix.S_IFMT != typ {
		if err := s.remove(dst); err != nil {
			return err
		}
		exists = false
	}
	switch typ {
	case unix.S_IFDIR:
		return s.syncDir(ctx, src, dst, rel, st, git, jobs)
	case unix.S_IFREG:
		if st.Nlink > 1 {
			return s.syncLinked(src, dst, rel, st, exists, &dstSt)
		}
		if exists && s.same(rel, st, &dstSt) {
			return nil
		}
		if s.live && git == gitMeta {
			// Copied here, not by a worker that may read it after the walk
			// has read objects/: every ref and index entry the copy holds
			// then names an object it holds too.
			return s.replaceFile(src, dst, st)
		}
		select {
		case jobs <- fileJob{src: src, dst: dst, st: *st}:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	case unix.S_IFLNK:
		if exists && s.sameLink(src, dst, st, &dstSt) {
			return nil
		}
		if exists {
			if err := s.remove(dst); err != nil {
				return err
			}
		}
		if err := s.copySymlink(src, dst, st); err != nil {
			return s.vanished(err)
		}
		s.changed.Add(1)
		return nil
	default:
		if exists && dstSt.Mode == st.Mode && dstSt.Rdev == st.Rdev && s.sameOwner(st, &dstSt) {
			return nil
		}
		if exists {
			if err := s.remove(dst); err != nil {
				return err
			}
		}
		if err := s.copySpecial(src, dst, st); err != nil {
			return err
		}
		s.changed.Add(1)
		return nil
	}
}

// syncLinked handles a file with more than one name: the first name a pass
// reaches is copied (when it changed) right here, before the walk can reach
// another; a later name is a link to that copy.
func (s *syncer) syncLinked(src, dst, rel string, st *unix.Stat_t, exists bool, dstSt *unix.Stat_t) error {
	key := [2]uint64{st.Dev, st.Ino}
	s.mu.Lock()
	first, seen := s.links[key]
	s.mu.Unlock()
	if seen {
		var firstSt unix.Stat_t
		if err := unix.Lstat(first, &firstSt); err != nil {
			return fmt.Errorf("stat %s: %w", first, err)
		}
		if exists && dstSt.Dev == firstSt.Dev && dstSt.Ino == firstSt.Ino {
			return nil
		}
		if exists {
			if err := s.remove(dst); err != nil {
				return err
			}
		}
		if err := unix.Link(first, dst); err != nil {
			return fmt.Errorf("link %s to %s: %w", dst, first, err)
		}
		s.stats.hardlinks.Add(1)
		s.changed.Add(1)
		return nil
	}
	if !exists || !s.same(rel, st, dstSt) {
		if err := s.replaceFile(src, dst, st); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(dst); err == nil {
		s.mu.Lock()
		s.links[key] = dst
		s.mu.Unlock()
	}
	return nil
}

// same reports whether the regular file dst (stat dstSt) is what the last
// pass copied of src as it is now (st).
func (s *syncer) same(rel string, st, dstSt *unix.Stat_t) bool {
	if dstSt.Size != st.Size || dstSt.Mode != st.Mode || dstSt.Mtim != st.Mtim || !s.sameOwner(st, dstSt) {
		return false
	}
	s.seenMu.Lock()
	ctime, ok := s.seen[rel]
	s.seenMu.Unlock()
	return ok && ctime == st.Ctim
}

func (s *syncer) sameOwner(st, dstSt *unix.Stat_t) bool {
	return s.singleUID || (st.Uid == dstSt.Uid && st.Gid == dstSt.Gid)
}

// sameLink reports whether the symlink dst is src (stat st).
func (s *syncer) sameLink(src, dst string, st, dstSt *unix.Stat_t) bool {
	if !s.sameOwner(st, dstSt) {
		return false
	}
	a, err := os.Readlink(src)
	if err != nil {
		return false
	}
	b, err := os.Readlink(dst)
	return err == nil && a == b
}

// replaceFile copies the regular file src over dst: into a temp name beside
// it, then renamed over it, so dst is never half written. A file the source
// lost meanwhile is skipped on a live copy.
func (s *syncer) replaceFile(src, dst string, st *unix.Stat_t) error {
	rel, _ := filepath.Rel(s.dst, dst)
	if s.onRead != nil {
		s.onRead(rel)
	}
	tmp := filepath.Join(filepath.Dir(dst), syncTempPrefix+strconv.FormatInt(s.tmpSeq.Add(1), 36))
	_ = os.Remove(tmp)
	if err := s.copyFile(src, tmp, st); err != nil {
		_ = os.Remove(tmp)
		return s.vanished(err)
	}
	if _, err := os.Lstat(tmp); err != nil {
		return nil // unreadable without root: the copier warned
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	// The self-check's expectation was made for the temp name.
	s.mu.Lock()
	for i := len(s.expects) - 1; i >= 0; i-- {
		if s.expects[i].dst == tmp {
			s.expects[i].dst = dst
			break
		}
	}
	s.mu.Unlock()
	s.changed.Add(1)
	// The change time the copy was made at, unless the file changed while it
	// was read: then the next pass copies it again.
	var after unix.Stat_t
	if err := unix.Lstat(src, &after); err != nil || after.Ctim != st.Ctim || after.Size != st.Size {
		s.seenMu.Lock()
		delete(s.seen, rel)
		s.seenMu.Unlock()
		return nil
	}
	s.seenMu.Lock()
	s.seen[rel] = st.Ctim
	s.seenMu.Unlock()
	return nil
}

// vanished is nil for err on a live copy whose source entry went away while
// it was copied; err otherwise.
func (s *syncer) vanished(err error) error {
	if s.live && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// remove deletes dst (a tree), counting it as a change.
func (s *syncer) remove(dst string) error {
	if !exists(dst) {
		return nil
	}
	if err := removeTree(dst); err != nil {
		return fmt.Errorf("delete %s: %w", dst, err)
	}
	s.changed.Add(1)
	return nil
}

// check is the copier's self-check of what the passes wrote that is still
// there: each file capability and setuid/setgid bit a pass copied, the last
// copy of each file only.
func (s *syncer) check() error {
	s.mu.Lock()
	last := map[string]int{}
	var keep []expectation
	for _, e := range s.expects {
		if _, err := os.Lstat(e.dst); err != nil {
			continue
		}
		if i, ok := last[e.dst]; ok {
			keep[i] = e
			continue
		}
		last[e.dst] = len(keep)
		keep = append(keep, e)
	}
	s.expects = keep
	s.mu.Unlock()
	return s.selfCheck()
}

// isGitDir reports whether dir (listing names) is a git repository's own
// dir: a .git dir, or a bare repository (HEAD, objects and refs).
func isGitDir(dir string, names []string) bool {
	if filepath.Base(dir) == ".git" {
		return true
	}
	have := 0
	for _, n := range names {
		switch n {
		case "HEAD", "objects", "refs":
			have++
		}
	}
	return have == 3
}

// orderEntries orders a dir's entries for a copy that must stay consistent
// with a running git. git writes an object before any ref or index entry that
// names it, and never rewrites one, so a git dir is read refs, HEAD and the
// index first (a live copy reads them as the walk reaches them, gitMeta) and
// objects/ last: every ref the copy holds then names objects it holds too. In
// a pack dir the pack goes before its index, which is what makes git see a
// pack at all.
func orderEntries(dir string, names []string, git bool) []string {
	out := slices.Clone(names)
	switch {
	case git:
		slices.SortStableFunc(out, func(a, b string) int { return boolOrder(a == "objects", b == "objects") })
	case filepath.Base(dir) == "pack" && filepath.Base(filepath.Dir(dir)) == "objects":
		rank := func(n string) int {
			switch {
			case strings.HasSuffix(n, ".pack"):
				return 0
			case strings.HasSuffix(n, ".idx"):
				return 2
			}
			return 1
		}
		slices.SortStableFunc(out, func(a, b string) int { return rank(a) - rank(b) })
	}
	return out
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// skipLiveGit names what a live copy leaves out of a git dir: the lock files a
// running git holds (a copied index.lock would stop every git in the clone)
// and its temp files.
func skipLiveGit(name string) bool {
	return strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, "tmp_") || strings.HasPrefix(name, ".tmp-")
}
