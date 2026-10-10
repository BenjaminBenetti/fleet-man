//go:build linux

package podseed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// defaultWorkers is how many files a copy writes at once: enough to hide a
// network disk's per-file latency, few enough for any node.
const defaultWorkers = 8

// copyStats counts what a copy wrote; read concurrently for progress.
type copyStats struct {
	files, bytes, hardlinks, caps, setuid atomic.Int64
}

// expectation is a file the self-check re-reads: one with a file capability or
// a setuid/setgid bit, the metadata a lossy copy drops first.
type expectation struct {
	dst  string
	caps []byte
	mode uint32
	uid  uint32
	gid  uint32
}

// dirFinal is a directory whose metadata is applied once everything in it is
// written (its times would move otherwise, and a read-only mode would block
// writing it).
type dirFinal struct {
	src, dst string
	st       unix.Stat_t
}

// fileJob is one regular file for a worker.
type fileJob struct {
	src, dst string
	st       unix.Stat_t
}

// copier copies image trees onto the PVC keeping docker's layer metadata:
// owner, mode with setuid/setgid/sticky, every xattr but security.selinux
// (file capabilities and ACLs included), hard links, holes, device nodes,
// FIFOs, sockets and times. Mounts in the source (the seed pod's own: the
// kubelet's /etc files, the PVC, the tools dir) are never descended into.
type copier struct {
	// srcRoot is the image root the source paths are relative to.
	srcRoot string
	rootDev uint64
	// mounts are the seed pod's mount points, as image paths.
	mounts map[string]bool
	// mountpoints are the instance's mount targets inside copied dirs: the
	// dir is created, its image content is not copied.
	mountpoints map[string]bool
	// singleUID is a copy without root: owners, device nodes, capabilities
	// and unreadable files cannot be kept, and are warnings, not errors.
	singleUID bool
	workers   int
	stats     *copyStats

	mu       sync.Mutex
	links    map[[2]uint64]string
	dirs     []dirFinal
	expects  []expectation
	warnings []string
}

func newCopier(srcRoot string, mounts, mountpoints map[string]bool, workers int, stats *copyStats) (*copier, error) {
	var st unix.Stat_t
	if err := unix.Stat(srcRoot, &st); err != nil {
		return nil, fmt.Errorf("stat the image root: %w", err)
	}
	if workers <= 0 {
		workers = defaultWorkers
	}
	return &copier{
		srcRoot:     srcRoot,
		rootDev:     st.Dev,
		mounts:      mounts,
		mountpoints: mountpoints,
		singleUID:   os.Geteuid() != 0,
		workers:     workers,
		stats:       stats,
		links:       map[[2]uint64]string{},
	}, nil
}

func (c *copier) warnf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

// imagePath is the image path of a source path.
func (c *copier) imagePath(src string) string {
	rel, err := filepath.Rel(c.srcRoot, src)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + rel
}

// copyTree copies the source dir src to dst, which must not exist yet (or be
// an empty dir), with src's own metadata on dst.
func (c *copier) copyTree(ctx context.Context, src, dst string) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan fileJob, 64)
	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := c.copyFile(j.src, j.dst, &j.st); err != nil {
					cancel(err)
				}
			}
		}()
	}

	var st unix.Stat_t
	err := unix.Lstat(src, &st)
	if err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		err = fmt.Errorf("%s is not a directory", src)
	}
	if err == nil {
		err = c.walk(ctx, src, dst, &st, jobs)
	}
	close(jobs)
	wg.Wait()
	if err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		return err
	}
	return c.finishDirs()
}

// walk copies the dir src (stat st) to dst: everything but regular files
// itself, regular files through the workers.
func (c *copier) walk(ctx context.Context, src, dst string, st *unix.Stat_t, jobs chan<- fileJob) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if err := unix.Mkdir(dst, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	c.mu.Lock()
	c.dirs = append(c.dirs, dirFinal{src: src, dst: dst, st: *st})
	c.mu.Unlock()

	img := c.imagePath(src)
	if st.Dev != c.rootDev || (img != "/" && c.mounts[img]) || c.mountpoints[img] {
		return nil
	}
	names, err := readDirNames(src)
	if err != nil {
		if c.singleUID && errors.Is(err, unix.EACCES) {
			c.warnf("%s: unreadable without root, left empty", img)
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	for _, name := range names {
		csrc, cdst := filepath.Join(src, name), filepath.Join(dst, name)
		var cst unix.Stat_t
		if err := unix.Lstat(csrc, &cst); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("stat %s: %w", csrc, err)
		}
		if err := failpoint("copy:" + c.imagePath(csrc)); err != nil {
			return err
		}
		typ := cst.Mode & unix.S_IFMT
		if typ != unix.S_IFDIR && c.mounts[c.imagePath(csrc)] {
			// A file the seed pod's runtime mounted (/etc/hosts and friends):
			// its image content is hidden here, and the seed writes a
			// placeholder.
			continue
		}
		switch typ {
		case unix.S_IFDIR:
			if err := c.walk(ctx, csrc, cdst, &cst, jobs); err != nil {
				return err
			}
		case unix.S_IFREG:
			if cst.Nlink > 1 {
				if done, err := c.hardlink(csrc, cdst, &cst); done || err != nil {
					if err != nil {
						return err
					}
					continue
				}
			}
			select {
			case jobs <- fileJob{src: csrc, dst: cdst, st: cst}:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		case unix.S_IFLNK:
			if err := c.copySymlink(csrc, cdst, &cst); err != nil {
				return err
			}
		default:
			if err := c.copySpecial(csrc, cdst, &cst); err != nil {
				return err
			}
		}
	}
	return nil
}

// hardlink handles a file with more than one link: a later name is linked to
// the first one's copy; the first is copied right here, before the walk can
// reach another of its names. done reports the file was handled.
func (c *copier) hardlink(src, dst string, st *unix.Stat_t) (done bool, err error) {
	key := [2]uint64{st.Dev, st.Ino}
	c.mu.Lock()
	first, seen := c.links[key]
	c.mu.Unlock()
	if seen {
		if err := unix.Link(first, dst); err != nil {
			return true, fmt.Errorf("link %s to %s: %w", dst, first, err)
		}
		c.stats.hardlinks.Add(1)
		return true, nil
	}
	if err := c.copyFile(src, dst, st); err != nil {
		return true, err
	}
	if _, err := os.Lstat(dst); err == nil {
		c.mu.Lock()
		c.links[key] = dst
		c.mu.Unlock()
	}
	return true, nil
}

// copyFile copies a regular file with its metadata.
func (c *copier) copyFile(src, dst string, st *unix.Stat_t) error {
	in, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if c.singleUID && errors.Is(err, os.ErrPermission) {
			c.warnf("%s: unreadable without root, not copied", c.imagePath(src))
			return nil
		}
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if st.Size > 0 && st.Blocks*512 < st.Size {
		err = copySparse(in, out, st.Size)
	} else {
		_, err = io.Copy(out, in)
	}
	if err == nil {
		err = c.applyMeta(src, dst, int(in.Fd()), int(out.Fd()), st)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = setTimes(dst, st)
	}
	if err != nil {
		return fmt.Errorf("copy %s: %w", c.imagePath(src), err)
	}
	c.stats.files.Add(1)
	c.stats.bytes.Add(st.Size)
	return nil
}

func (c *copier) copySymlink(src, dst string, st *unix.Stat_t) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", src, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("symlink %s: %w", dst, err)
	}
	if err := c.applyMeta(src, dst, -1, -1, st); err != nil {
		return fmt.Errorf("copy %s: %w", c.imagePath(src), err)
	}
	if err := setTimes(dst, st); err != nil {
		return fmt.Errorf("copy %s: %w", c.imagePath(src), err)
	}
	c.stats.files.Add(1)
	return nil
}

// copySpecial recreates a device node, FIFO or socket.
func (c *copier) copySpecial(src, dst string, st *unix.Stat_t) error {
	if err := unix.Mknod(dst, st.Mode, int(st.Rdev)); err != nil {
		if c.singleUID && errors.Is(err, unix.EPERM) {
			c.warnf("%s: device node not created without root", c.imagePath(src))
			return nil
		}
		return fmt.Errorf("mknod %s: %w", dst, err)
	}
	if err := c.applyMeta(src, dst, -1, -1, st); err != nil {
		return fmt.Errorf("copy %s: %w", c.imagePath(src), err)
	}
	if err := setTimes(dst, st); err != nil {
		return fmt.Errorf("copy %s: %w", c.imagePath(src), err)
	}
	c.stats.files.Add(1)
	return nil
}

// applyMeta gives dst src's owner, then its mode, then its xattrs: chown
// clears setuid bits and file capabilities, so it goes first. inFD and outFD
// are the open files of a regular file, -1 otherwise.
func (c *copier) applyMeta(src, dst string, inFD, outFD int, st *unix.Stat_t) error {
	typ := st.Mode & unix.S_IFMT
	isLink := typ == unix.S_IFLNK
	if !c.singleUID {
		var err error
		if outFD >= 0 {
			err = unix.Fchown(outFD, int(st.Uid), int(st.Gid))
		} else {
			err = unix.Lchown(dst, int(st.Uid), int(st.Gid))
		}
		if err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}
	if !isLink {
		var err error
		if outFD >= 0 {
			err = unix.Fchmod(outFD, st.Mode&0o7777)
		} else {
			err = unix.Fchmodat(unix.AT_FDCWD, dst, st.Mode&0o7777, 0)
		}
		if err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	caps, err := c.copyXattrs(src, dst, inFD, outFD, isLink)
	if err != nil {
		return err
	}
	if typ == unix.S_IFREG && (caps != nil || st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0) {
		if caps != nil {
			c.stats.caps.Add(1)
		}
		if st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
			c.stats.setuid.Add(1)
		}
		c.mu.Lock()
		c.expects = append(c.expects, expectation{dst: dst, caps: caps, mode: st.Mode & 0o7777, uid: st.Uid, gid: st.Gid})
		c.mu.Unlock()
	}
	return nil
}

// copyXattrs copies every extended attribute but security.selinux, returning
// the file capability it set (nil without one). An attribute the PVC cannot
// take is a warning, except a file capability with root, which is an error:
// the image's ping or newuidmap would quietly stop working.
func (c *copier) copyXattrs(src, dst string, inFD, outFD int, isLink bool) ([]byte, error) {
	names, err := listXattrs(src, inFD)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		return nil, fmt.Errorf("list xattrs: %w", err)
	}
	var caps []byte
	for _, name := range names {
		if skipXattr(name) {
			continue
		}
		value, err := getXattr(src, inFD, name)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return nil, fmt.Errorf("read xattr %s: %w", name, err)
		}
		if err := setXattr(dst, outFD, name, value); err != nil {
			if name == capabilityXattr && !c.singleUID {
				return nil, fmt.Errorf("set the file capability: %w", err)
			}
			if !(isLink && errors.Is(err, unix.EPERM)) {
				c.warnf("%s: xattr %s not kept: %v", c.imagePath(src), name, err)
			}
			continue
		}
		if name == capabilityXattr {
			caps = value
		}
	}
	return caps, nil
}

// finishDirs applies every copied dir's metadata, deepest first.
func (c *copier) finishDirs() error {
	c.mu.Lock()
	dirs := slices.Clone(c.dirs)
	c.dirs = nil
	c.mu.Unlock()
	sort.SliceStable(dirs, func(i, j int) bool { return len(dirs[i].dst) > len(dirs[j].dst) })
	for _, d := range dirs {
		if err := c.applyMeta(d.src, d.dst, -1, -1, &d.st); err != nil {
			return fmt.Errorf("copy %s: %w", c.imagePath(d.src), err)
		}
		if err := setTimes(d.dst, &d.st); err != nil {
			return fmt.Errorf("copy %s: %w", c.imagePath(d.src), err)
		}
	}
	return nil
}

// selfCheck re-reads every file capability and setuid/setgid bit the copy
// wrote and reports the ones that did not stick.
func (c *copier) selfCheck() error {
	c.mu.Lock()
	expects := slices.Clone(c.expects)
	c.mu.Unlock()
	var bad []string
	for _, e := range expects {
		var st unix.Stat_t
		if err := unix.Lstat(e.dst, &st); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", e.dst, err))
			continue
		}
		if st.Mode&0o7777 != e.mode {
			bad = append(bad, fmt.Sprintf("%s: mode %04o, want %04o", e.dst, st.Mode&0o7777, e.mode))
		}
		if !c.singleUID && (st.Uid != e.uid || st.Gid != e.gid) {
			bad = append(bad, fmt.Sprintf("%s: owner %d:%d, want %d:%d", e.dst, st.Uid, st.Gid, e.uid, e.gid))
		}
		if e.caps != nil {
			got, err := getXattr(e.dst, -1, capabilityXattr)
			if err != nil || string(got) != string(e.caps) {
				bad = append(bad, fmt.Sprintf("%s: file capability lost (%v)", e.dst, err))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	if len(bad) > 10 {
		bad = append(bad[:10], fmt.Sprintf("and %d more", len(bad)-10))
	}
	return fmt.Errorf("seed self-check: %s", strings.Join(bad, "; "))
}

// scan records the self-check's expectations for the tree src as it should
// be at dst, copying nothing: how a tree cp copied is checked.
func (c *copier) scan(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	img := c.imagePath(src)
	if st.Dev != c.rootDev || (img != "/" && c.mounts[img]) || c.mountpoints[img] {
		return nil
	}
	names, err := readDirNames(src)
	if err != nil {
		return err
	}
	for _, name := range names {
		csrc, cdst := filepath.Join(src, name), filepath.Join(dst, name)
		var cst unix.Stat_t
		if err := unix.Lstat(csrc, &cst); err != nil {
			continue
		}
		switch cst.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := c.scan(csrc, cdst); err != nil {
				return err
			}
		case unix.S_IFREG:
			if c.mounts[c.imagePath(csrc)] {
				continue
			}
			caps, err := getXattr(csrc, -1, capabilityXattr)
			if err != nil {
				caps = nil
			}
			if caps != nil || cst.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
				c.mu.Lock()
				c.expects = append(c.expects, expectation{dst: cdst, caps: caps, mode: cst.Mode & 0o7777, uid: cst.Uid, gid: cst.Gid})
				c.mu.Unlock()
			}
		}
	}
	return nil
}

// resetChecks drops the expectations and links recorded so far (before a dir
// is copied again by the fallback).
func (c *copier) resetChecks() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expects = nil
	c.links = map[[2]uint64]string{}
	c.dirs = nil
}

// readDirNames lists a directory's entries, sorted.
func readDirNames(dir string) ([]string, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// setTimes gives p (never followed) st's access and modification times.
func setTimes(p string, st *unix.Stat_t) error {
	ts := []unix.Timespec{st.Atim, st.Mtim}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("set times: %w", err)
	}
	return nil
}

// copySparse copies a file with holes, writing only its data extents. A
// filesystem that cannot report extents gets a plain copy.
func copySparse(in, out *os.File, size int64) error {
	if err := out.Truncate(size); err != nil {
		return err
	}
	fd := int(in.Fd())
	buf := make([]byte, 1<<20)
	var off int64
	for off < size {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		if err != nil {
			if off == 0 && errors.Is(err, unix.EINVAL) {
				_, err = io.Copy(out, in)
			}
			return err
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		for pos := data; pos < hole; {
			n := min(int64(len(buf)), hole-pos)
			k, err := in.ReadAt(buf[:n], pos)
			if k > 0 {
				if _, werr := out.WriteAt(buf[:k], pos); werr != nil {
					return werr
				}
				pos += int64(k)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
		off = hole
	}
	return nil
}
