//go:build linux

package podseed

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The root tests copy owners, file capabilities, root-owned setuid files,
// device nodes and ACLs for real, which needs root: CI runs the test binary
// under sudo with FLEET_PODSEED_REQUIRE_ROOT=1, which turns the skip into a
// failure.
//
//	go test -c -o /tmp/podseed.test ./internal/podseed
//	sudo FLEET_PODSEED_REQUIRE_ROOT=1 /tmp/podseed.test -test.v
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	if os.Getenv("FLEET_PODSEED_REQUIRE_ROOT") == "1" {
		t.Fatal("FLEET_PODSEED_REQUIRE_ROOT is set but this is not root")
	}
	t.Skip("needs root; see requireRoot")
}

// pingCaps is a v2 security.capability value: cap_net_raw, permitted and
// effective (what `setcap cap_net_raw+ep` writes).
func pingCaps() []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:], 0x02000001)
	binary.LittleEndian.PutUint32(b[4:], 1<<unix.CAP_NET_RAW)
	return b
}

// userACL is a system.posix_acl_access value granting uid 1234 read.
func userACL() []byte {
	type entry struct {
		tag, perm uint16
		id        uint32
	}
	entries := []entry{{0x01, 6, 0xffffffff}, {0x02, 4, 1234}, {0x04, 4, 0xffffffff}, {0x10, 4, 0xffffffff}, {0x20, 4, 0xffffffff}}
	b := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(b, 2)
	for i, e := range entries {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(b[off:], e.tag)
		binary.LittleEndian.PutUint16(b[off+2:], e.perm)
		binary.LittleEndian.PutUint32(b[off+4:], e.id)
	}
	return b
}

var fixtureTime = time.Date(2021, 6, 7, 8, 9, 10, 123456789, time.UTC)

// image is a fake image root for a seed to copy.
type image struct {
	root string
	// rootOnly reports which root-only fixtures were made (the fs may refuse
	// some, a kernel without SELinux xattrs say).
	caps, acl, selinux, trusted, device bool
}

// makeImage builds an image root with every kind of entry a seed must keep.
func makeImage(t *testing.T) *image {
	t.Helper()
	img := &image{root: filepath.Join(t.TempDir(), "image")}
	r := img.root
	dirs := []string{"etc", "usr/bin", "usr/lib", "var/lib/docker", "home/vscode", "run", "tmp", "opt/ro", "proc", "srv/empty"}
	for _, d := range dirs {
		mustMkdir(t, filepath.Join(r, d), 0o755)
	}
	files := map[string]string{
		"etc/passwd":                   imagePasswd,
		"etc/group":                    imageGroup,
		"etc/hosts":                    "the image's hosts, hidden by the kubelet's\n",
		"etc/os-release":               "ID=fixture\n",
		"usr/bin/tool":                 "#!/bin/sh\necho tool\n",
		"usr/bin/sudo":                 "pretend setuid binary",
		"usr/bin/ping":                 "pretend ping",
		"usr/lib/libx.so.1":            "shared object",
		"var/lib/docker/image-content": "under a volume target",
		"home/vscode/.bashrc":          "export PS1=x\n",
		"opt/ro/kept":                  "read-only dir content",
		"proc/never":                   "proc is never copied",
		"app.txt":                      "a top-level file",
	}
	for p, body := range files {
		if err := os.WriteFile(filepath.Join(r, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustChmod(t, filepath.Join(r, "usr/bin/tool"), 0o755)
	mustChmod(t, filepath.Join(r, "usr/bin/sudo"), 0o4755)
	mustChmod(t, filepath.Join(r, "home/vscode"), 0o750)
	if err := os.Link(filepath.Join(r, "usr/lib/libx.so.1"), filepath.Join(r, "usr/lib/libx.so")); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"bin": "usr/bin", "var/run": "../run", "etc/resolv.conf": "../run/resolv.conf", "usr/bin/dangling": "/nowhere"} {
		if err := os.Symlink(target, filepath.Join(r, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(r, "tmp/fifo"), 0o620); err != nil {
		t.Fatal(err)
	}
	// A sparse file: 8 MiB with data at its start and its end.
	sparse, err := os.Create(filepath.Join(r, "var/sparse.img"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sparse.WriteAt([]byte("head"), 0)
	_, _ = sparse.WriteAt([]byte("tail"), 8<<20-4)
	_ = sparse.Close()
	if err := unix.Setxattr(filepath.Join(r, "usr/bin/tool"), "user.fleet.test", []byte("kept"), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal(err)
	}

	if os.Geteuid() == 0 {
		mustChown(t, filepath.Join(r, "home/vscode"), 1000, 1000)
		mustChown(t, filepath.Join(r, "home/vscode/.bashrc"), 1000, 1000)
		mustChown(t, filepath.Join(r, "usr/lib/libx.so.1"), 1234, 5678)
		mustChown(t, filepath.Join(r, "usr/bin/dangling"), 1234, 5678)
		mustChmod(t, filepath.Join(r, "usr/bin/sudo"), 0o4755) // chown cleared nothing here, but be sure
		img.caps = unix.Setxattr(filepath.Join(r, "usr/bin/ping"), capabilityXattr, pingCaps(), 0) == nil
		img.acl = unix.Setxattr(filepath.Join(r, "etc/os-release"), "system.posix_acl_access", userACL(), 0) == nil
		img.selinux = unix.Setxattr(filepath.Join(r, "etc/os-release"), selinuxXattr, []byte("system_u:object_r:etc_t:s0\x00"), 0) == nil
		img.trusted = unix.Setxattr(filepath.Join(r, "etc/os-release"), "trusted.fleet", []byte("t"), 0) == nil
		img.device = unix.Mknod(filepath.Join(r, "srv/null"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))) == nil
	}
	mustChmod(t, filepath.Join(r, "opt/ro"), 0o555)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(r, "opt/ro"), 0o755) })
	// Times last: everything above moved them.
	ts := []unix.Timespec{unix.NsecToTimespec(fixtureTime.UnixNano()), unix.NsecToTimespec(fixtureTime.UnixNano())}
	_ = filepath.Walk(r, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
		}
		return nil
	})
	return img
}

func mustMkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, p string, mode uint32) {
	t.Helper()
	if err := unix.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustChown(t *testing.T, p string, uid, gid int) {
	t.Helper()
	if err := os.Lchown(p, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func lstat(t *testing.T, p string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return st
}

// fakeMountInfo points the seed at a mountinfo that lists mounts as the seed
// pod's runtime would have them (paths under the fixture image root).
func fakeMountInfo(t *testing.T, mountpoints ...string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "mountinfo")
	data := "1 0 0:1 / / rw - overlay overlay rw\n"
	for i, mp := range mountpoints {
		data += "2" + string(rune('0'+i)) + " 1 0:2 /x " + mp + " rw - ext4 /dev/sda rw\n"
	}
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountInfo
	mountInfo = file
	t.Cleanup(func() { mountInfo = old })
}
