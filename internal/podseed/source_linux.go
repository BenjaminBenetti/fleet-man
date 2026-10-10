//go:build linux

package podseed

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// mountInfo is the mountinfo the seed reads its own mounts from (tests point
// it elsewhere).
var mountInfo = "/proc/self/mountinfo"

// sourceMounts are the seed pod's mount points under srcRoot, as image paths
// ("/etc/hosts", "/fleet-pvc"): what the runtime and the kubelet put in the
// pod, which is no part of the image.
func sourceMounts(srcRoot string) (map[string]bool, error) {
	f, err := os.Open(mountInfo)
	if err != nil {
		return nil, fmt.Errorf("read the seed pod's mounts: %w", err)
	}
	defer f.Close()
	mounts := map[string]bool{}
	root := filepath.Clean(srcRoot)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountPath(fields[4])
		var img string
		switch {
		case root == "/":
			img = mp
		case strings.HasPrefix(mp, root+"/"):
			img = strings.TrimPrefix(mp, root)
		default:
			continue
		}
		if img != "/" {
			mounts[img] = true
		}
	}
	return mounts, sc.Err()
}

// unescapeMountPath decodes mountinfo's octal escapes (\040 for a space).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// classifyTops reads the image root's top-level entries: real dirs (copied),
// dirs the seed pod has something mounted on, and the rest, with the targets
// of the top-level symlinks.
func classifyTops(srcRoot string, mounts map[string]bool) (map[string]topKind, map[string]string, error) {
	var rootSt unix.Stat_t
	if err := unix.Stat(srcRoot, &rootSt); err != nil {
		return nil, nil, fmt.Errorf("stat the image root: %w", err)
	}
	names, err := readDirNames(srcRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read the image root: %w", err)
	}
	tops := map[string]topKind{}
	links := map[string]string{}
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(srcRoot, name), &st); err != nil {
			continue
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if st.Dev != rootSt.Dev || mounts["/"+name] {
				tops[name] = topMount
			} else {
				tops[name] = topDir
			}
		case unix.S_IFLNK:
			tops[name] = topOther
			if target, err := os.Readlink(filepath.Join(srcRoot, name)); err == nil {
				links[name] = target
			}
		default:
			tops[name] = topOther
		}
	}
	return tops, links, nil
}

// gnuCP is GNU cp, when the image has it: the fallback copier. busybox's cp
// keeps no xattrs, so it is no fallback.
func gnuCP() (string, bool) {
	cp, err := exec.LookPath("cp")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(cp, "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU coreutils") {
		return "", false
	}
	return cp, true
}

// cpTree copies src to the new dir dst with GNU cp, keeping what the Go
// copier keeps but security.selinux (cp leaves it alone without
// --preserve=context); without root it does not try to keep owners.
func cpTree(ctx context.Context, cp, src, dst string, singleUID bool) error {
	preserve := "--preserve=mode,ownership,timestamps,links,xattr"
	if singleUID {
		preserve = "--preserve=mode,timestamps,links,xattr"
	}
	cmd := exec.CommandContext(ctx, cp, "-dR", preserve, "--sparse=auto",
		"--one-file-system", "--", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp %s: %w: %s", src, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fallbackable reports whether copying again with cp could help: not for a
// full or over-quota disk, nor a cancelled job.
func fallbackable(err error) bool {
	return !errors.Is(err, unix.ENOSPC) && !errors.Is(err, unix.EDQUOT) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
