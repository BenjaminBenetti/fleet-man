//go:build linux

package podseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

// sharecopy_linux.go is an OpShareCopy: a fleet share copied whole onto
// another volume, when the share moves to another StorageClass (the
// connection's shared class changed). The share's tree (the agents' state,
// shared files, custom mounts, the spoke's own sync state) is copied as
// it is, as the seed copies an image: owners, modes with setuid and sticky
// bits, extended attributes but the SELinux label, hard links, holes,
// symlinks (never followed) and times. The volume's own lost+found is
// neither copied nor removed. A copy is then checked against its source:
// the same entries of the same kinds, sizes, modes and owners (owners only
// as root), the same symlink targets.

// shareCopyLostFound is the folder an ext4 volume's root has of its own.
const shareCopyLostFound = "lost+found"

// shareCopyRun is one OpShareCopy.
type shareCopyRun struct {
	spec  JobSpec
	copy  ShareCopySpec
	srv   *jobboot.Server
	logf  func(string, ...any)
	stats copyStats
}

func (r *shareCopyRun) progress(done bool) {
	r.srv.Progress(jobboot.Progress{Phase: "share-copy", Bytes: r.stats.bytes.Load(), Files: r.stats.files.Load(), Done: done})
}

func (r *shareCopyRun) run(ctx context.Context) jobOutcome {
	src, dst := r.copy.Source, r.spec.PVC
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	tctx, stopTicks := context.WithCancel(ctx)
	defer stopTicks()
	go func() {
		for {
			select {
			case <-tick.C:
				r.progress(false)
			case <-tctx.Done():
				return
			}
		}
	}()
	res, err := copyShare(ctx, src, dst, r.copy.Workers, &r.stats, r.logf)
	if err != nil {
		return jobOutcome{err: err}
	}
	if err := syncFS(dst); err != nil {
		return jobOutcome{err: fmt.Errorf("sync the copy: %w", err)}
	}
	r.progress(true)
	data, err := json.Marshal(res)
	if err != nil {
		return jobOutcome{err: err}
	}
	r.logf("copied %d files, %d dirs, %d bytes", res.Files, res.Dirs, res.Bytes)
	return jobOutcome{bytes: res.Bytes, output: data, sha: sha256Hex(data)}
}

// copyShare replaces what dst holds with a copy of the share tree at src,
// and checks it.
func copyShare(ctx context.Context, src, dst string, workers int, stats *copyStats, logf func(string, ...any)) (ShareCopyResult, error) {
	if err := emptyDirBut(dst, shareCopyLostFound); err != nil {
		return ShareCopyResult{}, fmt.Errorf("empty the copy's volume: %w", err)
	}
	c, err := newCopier(src, map[string]bool{"/" + shareCopyLostFound: true}, nil, workers, stats)
	if err != nil {
		return ShareCopyResult{}, fmt.Errorf("the share: %w", err)
	}
	if err := c.copyTree(ctx, src, dst); err != nil {
		return ShareCopyResult{}, fmt.Errorf("copy the share: %w", err)
	}
	for _, w := range c.warnings {
		logf("warning: %s", w)
	}
	if err := c.selfCheck(); err != nil {
		return ShareCopyResult{}, err
	}
	res, err := compareShare(src, dst, !c.singleUID)
	if err != nil {
		return ShareCopyResult{}, fmt.Errorf("the copy is not the share: %w", err)
	}
	return res, nil
}

// emptyDirBut removes everything in dir but keep.
func emptyDirBut(dir, keep string) error {
	names, err := readDirNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// compareShare checks that dst holds what src does: every entry (but the
// root's lost+found) of the same kind, mode, size and symlink target, and
// with owners the same owners. It returns what src holds.
func compareShare(src, dst string, owners bool) (ShareCopyResult, error) {
	var res ShareCopyResult
	var bad []string
	note := func(format string, args ...any) {
		if len(bad) < 10 {
			bad = append(bad, fmt.Sprintf(format, args...))
		} else if len(bad) == 10 {
			bad = append(bad, "and more")
		}
	}
	var walk func(rel string) error
	walk = func(rel string) error {
		names, err := readDirNames(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		dstNames, err := readDirNames(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, name := range names {
			if rel == "" && name == shareCopyLostFound {
				continue
			}
			want[name] = true
		}
		for _, name := range dstNames {
			if !want[name] && !(rel == "" && name == shareCopyLostFound) {
				note("%s: in the copy only", filepath.Join(rel, name))
			}
		}
		for _, name := range names {
			if !want[name] {
				continue
			}
			p := filepath.Join(rel, name)
			var s, d unix.Stat_t
			if err := unix.Lstat(filepath.Join(src, p), &s); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				return err
			}
			if err := unix.Lstat(filepath.Join(dst, p), &d); err != nil {
				note("%s: missing from the copy", p)
				continue
			}
			typ := s.Mode & unix.S_IFMT
			switch {
			case d.Mode&unix.S_IFMT != typ:
				note("%s: a %s in the copy", p, kindOf(d.Mode))
				continue
			case typ != unix.S_IFLNK && d.Mode&0o7777 != s.Mode&0o7777:
				note("%s: mode %04o, the share's %04o", p, d.Mode&0o7777, s.Mode&0o7777)
			case owners && (d.Uid != s.Uid || d.Gid != s.Gid):
				note("%s: owner %d:%d, the share's %d:%d", p, d.Uid, d.Gid, s.Uid, s.Gid)
			}
			switch typ {
			case unix.S_IFDIR:
				res.Dirs++
				if err := walk(p); err != nil {
					return err
				}
			case unix.S_IFREG:
				res.Files++
				res.Bytes += s.Size
				if d.Size != s.Size {
					note("%s: %d bytes, the share's %d", p, d.Size, s.Size)
				}
			case unix.S_IFLNK:
				a, aerr := os.Readlink(filepath.Join(src, p))
				b, berr := os.Readlink(filepath.Join(dst, p))
				if aerr != nil || berr != nil || a != b {
					note("%s: links to %q, the share's to %q", p, b, a)
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return res, err
	}
	if len(bad) > 0 {
		return res, errors.New(strings.Join(bad, "; "))
	}
	return res, nil
}

// kindOf names a file type.
func kindOf(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFREG:
		return "file"
	case unix.S_IFLNK:
		return "symlink"
	case unix.S_IFSOCK:
		return "socket"
	case unix.S_IFIFO:
		return "fifo"
	}
	return "device"
}
