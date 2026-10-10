//go:build linux

package podseed

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// copyImage copies the fixture's top-level dirs the way a seed does.
func copyImage(t *testing.T, img *image, mountpoints map[string]bool) (string, *copier) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "root")
	mustMkdir(t, dst, 0o755)
	mounts, err := sourceMounts(img.root)
	if err != nil {
		t.Fatal(err)
	}
	var stats copyStats
	var last *copier
	for _, top := range []string{"etc", "usr", "var", "home", "run", "tmp", "opt", "srv"} {
		c, err := newCopier(img.root, mounts, mountpoints, 3, &stats)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.copyTree(context.Background(), filepath.Join(img.root, top), filepath.Join(dst, top)); err != nil {
			t.Fatalf("copy /%s: %v", top, err)
		}
		if err := c.selfCheck(); err != nil {
			t.Fatalf("self-check /%s: %v", top, err)
		}
		last = c
	}
	t.Cleanup(func() { _ = removeTree(dst) })
	return dst, last
}

func TestCopyKeepsTheLayersMetadata(t *testing.T) {
	img := makeImage(t)
	fakeMountInfo(t, filepath.Join(img.root, "etc/hosts"))
	dst, _ := copyImage(t, img, map[string]bool{"/var/lib/docker": true})
	src := img.root

	for _, p := range []string{"etc/passwd", "usr/bin/tool", "usr/lib/libx.so.1", "home/vscode/.bashrc", "opt/ro/kept"} {
		a, _ := os.ReadFile(filepath.Join(src, p))
		b, err := os.ReadFile(filepath.Join(dst, p))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s: content differs (%v)", p, err)
		}
	}
	for _, p := range []string{"usr/bin/tool", "usr/bin/sudo", "home/vscode", "opt/ro", "tmp/fifo", "etc", "var/run", "usr/bin/dangling"} {
		s, d := lstat(t, filepath.Join(src, p)), lstat(t, filepath.Join(dst, p))
		if s.Mode != d.Mode {
			t.Errorf("%s: mode %o, want %o", p, d.Mode, s.Mode)
		}
		if s.Mtim != d.Mtim {
			t.Errorf("%s: mtime %v, want %v", p, d.Mtim, s.Mtim)
		}
		if os.Geteuid() == 0 && (s.Uid != d.Uid || s.Gid != d.Gid) {
			t.Errorf("%s: owner %d:%d, want %d:%d", p, d.Uid, d.Gid, s.Uid, s.Gid)
		}
	}
	if st := lstat(t, filepath.Join(dst, "usr/bin/sudo")); st.Mode&unix.S_ISUID == 0 {
		t.Error("the setuid bit was lost")
	}
	for link, want := range map[string]string{"var/run": "../run", "etc/resolv.conf": "../run/resolv.conf", "usr/bin/dangling": "/nowhere"} {
		if got, err := os.Readlink(filepath.Join(dst, link)); err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", link, got, err, want)
		}
	}
	a, b := lstat(t, filepath.Join(dst, "usr/lib/libx.so")), lstat(t, filepath.Join(dst, "usr/lib/libx.so.1"))
	if a.Ino != b.Ino || a.Nlink != 2 {
		t.Errorf("hard link not kept: inodes %d %d, nlink %d", a.Ino, b.Ino, a.Nlink)
	}
	sp := lstat(t, filepath.Join(dst, "var/sparse.img"))
	if sp.Size != 8<<20 || sp.Blocks*512 >= sp.Size/2 {
		t.Errorf("the sparse file came out %d bytes in %d blocks", sp.Size, sp.Blocks)
	}
	data, _ := os.ReadFile(filepath.Join(dst, "var/sparse.img"))
	if !bytes.HasPrefix(data, []byte("head")) || !bytes.HasSuffix(data, []byte("tail")) {
		t.Error("the sparse file's data differs")
	}
	if v, err := getXattr(filepath.Join(src, "usr/bin/tool"), -1, "user.fleet.test"); err == nil {
		if got, err := getXattr(filepath.Join(dst, "usr/bin/tool"), -1, "user.fleet.test"); err != nil || !bytes.Equal(got, v) {
			t.Errorf("user xattr: %q %v", got, err)
		}
	}
	// The kubelet's mounted /etc/hosts is not the image's: skipped (the seed
	// writes a placeholder).
	if _, err := os.Lstat(filepath.Join(dst, "etc/hosts")); !os.IsNotExist(err) {
		t.Errorf("a mounted file was copied: %v", err)
	}
	// A volume target's dir exists, its image content does not.
	if fi, err := os.Stat(filepath.Join(dst, "var/lib/docker")); err != nil || !fi.IsDir() {
		t.Errorf("the mountpoint dir is missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "var/lib/docker/image-content")); !os.IsNotExist(err) {
		t.Errorf("content under a mount target was copied: %v", err)
	}
}

func TestCopyAsRootKeepsOwnersCapabilitiesDevicesAndACLs(t *testing.T) {
	requireRoot(t)
	img := makeImage(t)
	fakeMountInfo(t)
	dst, c := copyImage(t, img, nil)
	if c.singleUID {
		t.Fatal("a root copy ran in single-uid mode")
	}
	if st := lstat(t, filepath.Join(dst, "usr/lib/libx.so.1")); st.Uid != 1234 || st.Gid != 5678 {
		t.Errorf("owner %d:%d", st.Uid, st.Gid)
	}
	if st := lstat(t, filepath.Join(dst, "usr/bin/dangling")); st.Uid != 1234 {
		t.Errorf("a symlink's owner %d", st.Uid)
	}
	if st := lstat(t, filepath.Join(dst, "usr/bin/sudo")); st.Uid != 0 || st.Mode&0o7777 != 0o4755 {
		t.Errorf("setuid root file: owner %d mode %o", st.Uid, st.Mode&0o7777)
	}
	if !img.caps {
		t.Fatal("this filesystem refused a file capability on the fixture; the test cannot check them")
	}
	if got, err := getXattr(filepath.Join(dst, "usr/bin/ping"), -1, capabilityXattr); err != nil || !bytes.Equal(got, pingCaps()) {
		t.Errorf("file capability: %x %v", got, err)
	}
	if img.device {
		st := lstat(t, filepath.Join(dst, "srv/null"))
		if st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != unix.Mkdev(1, 3) {
			t.Errorf("device node: mode %o rdev %d", st.Mode, st.Rdev)
		}
	} else {
		t.Log("mknod refused here; device nodes unchecked")
	}
	if img.acl {
		if got, err := getXattr(filepath.Join(dst, "etc/os-release"), -1, "system.posix_acl_access"); err != nil || !bytes.Equal(got, userACL()) {
			t.Errorf("ACL: %x %v", got, err)
		}
	} else {
		t.Log("ACLs refused on this filesystem; unchecked")
	}
	if img.trusted {
		if got, err := getXattr(filepath.Join(dst, "etc/os-release"), -1, "trusted.fleet"); err != nil || string(got) != "t" {
			t.Errorf("trusted xattr: %q %v", got, err)
		}
	}
	if img.selinux {
		// The node's SELinux, where there is one, labels the new file itself;
		// what must never arrive is the image's label.
		want, _ := getXattr(filepath.Join(img.root, "etc/os-release"), -1, selinuxXattr)
		if got, err := getXattr(filepath.Join(dst, "etc/os-release"), -1, selinuxXattr); err == nil && bytes.Equal(got, want) {
			t.Errorf("security.selinux %q was copied", got)
		}
	} else {
		t.Log("security.selinux cannot be set here; only skipXattr is checked")
	}
	if !skipXattr(selinuxXattr) || skipXattr(capabilityXattr) {
		t.Fatal("skipXattr")
	}
	if c.stats.caps.Load() < 1 || c.stats.setuid.Load() < 1 {
		t.Errorf("stats caps %d setuid %d", c.stats.caps.Load(), c.stats.setuid.Load())
	}
}

func TestSelfCheckCatchesLostMetadata(t *testing.T) {
	img := makeImage(t)
	fakeMountInfo(t)
	dst := filepath.Join(t.TempDir(), "usr")
	var stats copyStats
	c, err := newCopier(img.root, nil, nil, 2, &stats)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.copyTree(context.Background(), filepath.Join(img.root, "usr"), dst); err != nil {
		t.Fatal(err)
	}
	if err := c.selfCheck(); err != nil {
		t.Fatal(err)
	}
	// What a lossy copy (a filesystem dropping setuid bits, an old cp) does.
	mustChmod(t, filepath.Join(dst, "bin/sudo"), 0o755)
	err = c.selfCheck()
	if err == nil || !strings.Contains(err.Error(), "bin/sudo: mode 0755, want 4755") {
		t.Fatalf("self-check = %v", err)
	}
	// scan rebuilds the same expectations from the source, for a tree cp
	// copied.
	c.resetChecks()
	if err := c.scan(filepath.Join(img.root, "usr"), dst); err != nil {
		t.Fatal(err)
	}
	if err := c.selfCheck(); err == nil {
		t.Fatal("the scan's self-check missed the lost setuid bit")
	}
}

func TestCopyFailsWhenTheDestinationExists(t *testing.T) {
	img := makeImage(t)
	fakeMountInfo(t)
	dst := filepath.Join(t.TempDir(), "etc")
	mustMkdir(t, dst, 0o755)
	if err := os.WriteFile(filepath.Join(dst, "passwd"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := newCopier(img.root, nil, nil, 2, &copyStats{})
	err := c.copyTree(context.Background(), filepath.Join(img.root, "etc"), dst)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy over an existing file: %v", err)
	}
	if !fallbackable(err) || fallbackable(unix.ENOSPC) || fallbackable(context.Canceled) {
		t.Fatal("fallbackable")
	}
}

func TestCopyIsCancellable(t *testing.T) {
	img := makeImage(t)
	fakeMountInfo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := newCopier(img.root, nil, nil, 2, &copyStats{})
	if err := c.copyTree(ctx, filepath.Join(img.root, "usr"), filepath.Join(t.TempDir(), "usr")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled copy: %v", err)
	}
}

func TestSourceMountsAndTops(t *testing.T) {
	img := makeImage(t)
	file := filepath.Join(t.TempDir(), "mountinfo")
	data := "22 1 0:1 / / rw - overlay overlay rw\n" +
		"23 22 0:2 / /proc rw - proc proc rw\n" +
		"24 22 8:1 /x " + img.root + "/etc/hosts rw - ext4 /dev/sda rw\n" +
		"25 22 8:1 /y " + img.root + "/with\\040space rw - ext4 /dev/sda rw\n" +
		"26 22 8:1 /z " + img.root + "/srv rw - ext4 /dev/sda rw\n"
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountInfo
	mountInfo = file
	defer func() { mountInfo = old }()
	mounts, err := sourceMounts(img.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/etc/hosts", "/with space", "/srv"} {
		if !mounts[want] {
			t.Errorf("mounts %v lack %s", mounts, want)
		}
	}
	if mounts["/proc"] || mounts["/"] {
		t.Errorf("mounts outside the image root leaked in: %v", mounts)
	}
	all, err := sourceMounts("/")
	if err != nil || !all["/proc"] || all["/"] {
		t.Errorf("for /: %v %v", all, err)
	}
	tops, links, err := classifyTops(img.root, mounts)
	if err != nil {
		t.Fatal(err)
	}
	if tops["etc"] != topDir || tops["srv"] != topMount || tops["bin"] != topOther || tops["app.txt"] != topOther {
		t.Errorf("tops %v", tops)
	}
	if links["bin"] != "usr/bin" {
		t.Errorf("links %v", links)
	}
}
