package podagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type entry struct {
	hdr  tar.Header
	body string
}

func bundleOf(t *testing.T, entries ...entry) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// tmuxLike is shaped like the real bundle: a binary, terminfo and a hard link.
func tmuxLike(t *testing.T, version string) ([]byte, string) {
	return bundleOf(t,
		entry{hdr: tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}},
		entry{hdr: tar.Header{Name: "bin/tmux", Typeflag: tar.TypeReg, Mode: 0o755}, body: "#!/bin/sh\necho tmux " + version + "\n"},
		entry{hdr: tar.Header{Name: "share/terminfo/i/iTerm2.app", Typeflag: tar.TypeReg}, body: "terminfo " + version},
		entry{hdr: tar.Header{Name: "share/terminfo/i/iterm2", Typeflag: tar.TypeLink, Linkname: "share/terminfo/i/iTerm2.app"}},
		entry{hdr: tar.Header{Name: "share/terminfo/x/xterm", Typeflag: tar.TypeSymlink, Linkname: "../i/iTerm2.app"}},
	)
}

func TestInstallBundle(t *testing.T) {
	dir := t.TempDir()
	data1, sha1 := tmuxLike(t, "3.8")
	o1 := BundleOptions{Dir: dir, Name: "tmux", SHA256: sha1}
	if BundleInstalled(o1) {
		t.Fatal("installed before the install")
	}
	if err := InstallBundle(bytes.NewReader(data1), o1); err != nil {
		t.Fatal(err)
	}
	if !BundleInstalled(o1) {
		t.Fatal("not installed after the install")
	}
	if target, _ := os.Readlink(filepath.Join(dir, "tmux")); target != "tmux-"+sha1 {
		t.Fatalf("link -> %q", target)
	}
	bin := filepath.Join(dir, "tmux", "bin", "tmux")
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("bin/tmux: %v %v", fi, err)
	}
	a, _ := os.Stat(filepath.Join(dir, "tmux", "share/terminfo/i/iTerm2.app"))
	b, _ := os.Stat(filepath.Join(dir, "tmux", "share/terminfo/i/iterm2"))
	if a == nil || b == nil || !os.SameFile(a, b) {
		t.Fatal("the hard link is not a hard link")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "tmux", "share/terminfo/x/xterm")); err != nil || string(got) != "terminfo 3.8" {
		t.Fatalf("the symlink reads %q %v", got, err)
	}
	// Installed: nothing is read.
	if err := InstallBundle(failingReader{t}, o1); err != nil {
		t.Fatal(err)
	}

	// A new bundle replaces the link and keeps the previous tree; a third
	// deletes the oldest.
	data2, sha2 := tmuxLike(t, "3.9")
	if err := InstallBundle(bytes.NewReader(data2), BundleOptions{Dir: dir, Name: "tmux", SHA256: sha2}); err != nil {
		t.Fatal(err)
	}
	data3, sha3 := tmuxLike(t, "4.0")
	if err := InstallBundle(bytes.NewReader(data3), BundleOptions{Dir: dir, Name: "tmux", SHA256: sha3}); err != nil {
		t.Fatal(err)
	}
	var trees []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		trees = append(trees, e.Name())
	}
	want := map[string]bool{"tmux": true, "tmux-" + sha2: true, "tmux-" + sha3: true}
	if len(trees) != len(want) {
		t.Fatalf("dir holds %v, want the link, the current and the previous tree", trees)
	}
	for _, n := range trees {
		if !want[n] {
			t.Fatalf("dir holds %v, want the link, the current and the previous tree", trees)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "tmux", "share/terminfo/i/iterm2")); string(got) != "terminfo 4.0" {
		t.Fatalf("current tree reads %q", got)
	}
}

type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Fatal("read an installed bundle's archive")
	return 0, nil
}

func TestInstallBundleRefusals(t *testing.T) {
	good, _ := tmuxLike(t, "3.8")
	cases := map[string][]entry{
		"escape":         {{hdr: tar.Header{Name: "../x", Typeflag: tar.TypeReg}}},
		"absolute":       {{hdr: tar.Header{Name: "/etc/x", Typeflag: tar.TypeReg}}},
		"symlink out":    {{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}}},
		"symlink abs":    {{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
		"link missing":   {{hdr: tar.Header{Name: "a", Typeflag: tar.TypeLink, Linkname: "nothing"}}},
		"link escape":    {{hdr: tar.Header{Name: "a", Typeflag: tar.TypeLink, Linkname: "../x"}}},
		"device":         {{hdr: tar.Header{Name: "a", Typeflag: tar.TypeChar}}},
		"through a link": {{hdr: tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: "."}}, {hdr: tar.Header{Name: "d/../../x", Typeflag: tar.TypeReg}}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data, sha := bundleOf(t, entries...)
			if err := InstallBundle(bytes.NewReader(data), BundleOptions{Dir: dir, Name: "tmux", SHA256: sha}); err == nil {
				t.Fatal("installed")
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Fatalf("a refused bundle left %v", left)
			}
		})
	}

	dir := t.TempDir()
	if err := InstallBundle(bytes.NewReader(good), BundleOptions{Dir: dir, Name: "tmux", SHA256: strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a mismatched sha256: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a mismatched bundle left %v", left)
	}
	for _, o := range []BundleOptions{
		{Dir: "rel", Name: "tmux", SHA256: strings.Repeat("a", 64)},
		{Dir: dir, Name: "../x", SHA256: strings.Repeat("a", 64)},
		{Dir: dir, Name: "tmux", SHA256: "abc"},
	} {
		if err := InstallBundle(bytes.NewReader(good), o); err == nil {
			t.Errorf("%+v installed", o)
		}
	}
}

// The real static tmux bundle (FLEET_TEST_TMUX_BUNDLE, `make pod-tmux`)
// installs and runs from the link, its terminfo found where it was compiled
// to look.
func TestInstallTheRealTmuxBundle(t *testing.T) {
	path := os.Getenv("FLEET_TEST_TMUX_BUNDLE")
	if path == "" {
		t.Skip("set FLEET_TEST_TMUX_BUNDLE=<fleet-tmux-linux-<arch>.tar.gz> to install the real bundle")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	dir := t.TempDir()
	o := BundleOptions{Dir: dir, Name: "tmux", SHA256: hex.EncodeToString(sum[:])}
	if err := InstallBundle(bytes.NewReader(data), o); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" || !strings.Contains(path, runtime.GOARCH) {
		return
	}
	out, err := exec.Command(filepath.Join(dir, "tmux", "bin", "tmux"), "-V").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "tmux ") {
		t.Fatalf("tmux -V: %q %v", out, err)
	}
}
