package podseed

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name     string
	typ      byte
	mode     int64
	body     string
	linkname string
	uid, gid int
}

func makeTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.linkname,
			ModTime: mtime, Uid: e.uid, Gid: e.gid, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractWorkspace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ws, "ro"), 0o755) })
	// Left by an earlier, failed transfer.
	if err := os.MkdirAll(filepath.Join(ws, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := makeTar(t, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./README.md", typ: tar.TypeReg, mode: 0o644, body: "hello\n"},
		{name: "./bin/run.sh", typ: tar.TypeReg, mode: 0o750, body: "#!/bin/sh\n"}, // parent listed later
		{name: "./bin/", typ: tar.TypeDir, mode: 0o700},
		{name: "./ro/", typ: tar.TypeDir, mode: 0o555},
		{name: "./ro/kept", typ: tar.TypeReg, mode: 0o444, body: "x"},
		{name: "./link", typ: tar.TypeSymlink, linkname: "README.md"},
		{name: "./outside", typ: tar.TypeSymlink, linkname: "/etc/passwd"}, // data, never followed
		{name: "./hard", typ: tar.TypeLink, linkname: "./README.md"},
		{name: "./README.md", typ: tar.TypeReg, mode: 0o644, body: "hello again\n"}, // later wins
		{name: "./deep/a/b/c.txt", typ: tar.TypeReg, mode: 0o600, body: "deep"},
		{name: "./fifo", typ: tar.TypeFifo, mode: 0o644},
	})
	var reports int
	st, err := ExtractWorkspace(bytes.NewReader(data), ws, nil, func(WorkspaceStats) { reports++ })
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 8 || st.Skipped != 1 {
		t.Fatalf("stats %+v", st)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(ws, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	if read("README.md") != "hello again\n" || read("bin/run.sh") != "#!/bin/sh\n" || read("deep/a/b/c.txt") != "deep" {
		t.Fatal("contents differ")
	}
	if read("hard") != "hello\n" {
		t.Fatalf("hard link: %q (it links the first README.md, which the later entry replaced)", read("hard"))
	}
	if target, _ := os.Readlink(filepath.Join(ws, "outside")); target != "/etc/passwd" {
		t.Fatalf("symlink target %q", target)
	}
	if _, err := os.Stat(filepath.Join(ws, "stale")); !os.IsNotExist(err) {
		t.Fatal("the stale dir survived")
	}
	for p, want := range map[string]os.FileMode{"bin": 0o700, "bin/run.sh": 0o750, "ro": 0o555, "ro/kept": 0o444, "deep/a": 0o755, ".": 0o755} {
		fi, err := os.Lstat(filepath.Join(ws, p))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v %v, want %v", p, fi.Mode().Perm(), err, want)
		}
	}
	fi, _ := os.Stat(filepath.Join(ws, "bin"))
	if !fi.ModTime().Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("dir mtime %v", fi.ModTime())
	}
}

func TestExtractWorkspaceRefusesToLeaveTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]tarEntry{
		"dotdot":        {{name: "../escape", typ: tar.TypeReg, body: "x"}},
		"nested dotdot": {{name: "a/../../escape", typ: tar.TypeReg, body: "x"}},
		"through a link": {
			{name: "out", typ: tar.TypeSymlink, linkname: outside},
			{name: "out/escape", typ: tar.TypeReg, body: "x"},
		},
		"relative link out": {
			{name: "up", typ: tar.TypeSymlink, linkname: "../.."},
			{name: "up/escape", typ: tar.TypeReg, body: "x"},
		},
		"hard link out": {{name: "h", typ: tar.TypeLink, linkname: "../../etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), "ws")
			if _, err := ExtractWorkspace(bytes.NewReader(makeTar(t, entries)), ws, nil, nil); err == nil {
				t.Fatal("an escaping entry was extracted")
			}
			if _, err := os.Stat(filepath.Join(outside, "escape")); err == nil {
				t.Fatal("a file landed outside the workspace")
			}
		})
	}
}

// failingReader yields data, then err in place of io.EOF: a WorkspaceStream
// whose checksum did not match.
type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

func TestExtractWorkspaceSeesTheStreamsVerdict(t *testing.T) {
	data := makeTar(t, []tarEntry{{name: "a", typ: tar.TypeReg, body: "a"}})
	verdict := errors.New("workspace checksum mismatch")
	// The tar is complete; only the end of the stream says it is not to be
	// trusted.
	_, err := ExtractWorkspace(&failingReader{r: bytes.NewReader(append(data, make([]byte, 4096)...)), err: verdict},
		filepath.Join(t.TempDir(), "ws"), nil, nil)
	if !errors.Is(err, verdict) {
		t.Fatalf("err %v, want the stream's verdict", err)
	}
	if _, err := ExtractWorkspace(strings.NewReader("not a tar at all, not even close........"), filepath.Join(t.TempDir(), "ws"), nil, nil); err == nil {
		t.Fatal("garbage extracted")
	}
}

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	write := func(name, content string) (string, string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return p, sha256Hex([]byte(content))
	}
	v1, sha1 := write("v1", "fleet v1")
	v2, sha2 := write("v2", "fleet v2")
	v3, sha3 := write("v3", "fleet v3")
	link := func() string {
		target, _ := os.Readlink(filepath.Join(bin, "fleet"))
		return target
	}

	if prev, err := installBinary(v1, sha1, bin); err != nil || prev != "" || link() != BinaryName(sha1) {
		t.Fatalf("first install: prev %q err %v link %q", prev, err, link())
	}
	if prev, err := installBinary(v1, sha1, bin); err != nil || prev != "" || link() != BinaryName(sha1) {
		t.Fatalf("reinstall: prev %q err %v", prev, err)
	}
	if prev, err := installBinary(v2, sha2, bin); err != nil || prev != sha1 || link() != BinaryName(sha2) {
		t.Fatalf("upgrade: prev %q err %v link %q", prev, err, link())
	}
	if _, err := installBinary(v3, sha3, bin); err != nil {
		t.Fatal(err)
	}
	// The current binary and the one before it stay, for pod-init's fallback.
	if got := entries(t, bin); got != "fleet "+BinaryName(sha2)+" "+BinaryName(sha3) {
		got2 := "fleet " + BinaryName(sha3) + " " + BinaryName(sha2)
		if got != got2 {
			t.Fatalf("bin dir: %s", got)
		}
	}
	if fi, _ := os.Stat(filepath.Join(bin, BinaryName(sha3))); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := installBinary(v1, sha256Hex([]byte("fleet v4")), bin); err == nil {
		t.Fatal("a binary whose sha differs from the one named was installed")
	}
}

func TestLinkFleet(t *testing.T) {
	g := makeGen(t, map[string]string{"usr/bin/fleet": "an old copy of fleet the image shipped"}, map[string]string{"bin": "usr/bin"})
	if ok, err := linkFleet(g); !ok || err != nil {
		t.Fatalf("link: %v %v", ok, err)
	}
	if target, _ := os.Readlink(filepath.Join(g.pvc, DirRoot, "usr/bin/fleet")); target != CurrentBinary {
		t.Fatalf("link target %q", target)
	}
	if ok, err := linkFleet(g); !ok || err != nil {
		t.Fatalf("relink: %v %v", ok, err)
	}
	none := makeGen(t, map[string]string{"etc/x": "x"}, nil)
	if ok, err := linkFleet(none); ok || err != nil {
		t.Fatalf("an image without /usr/bin: %v %v", ok, err)
	}
	// /usr/bin a symlink out of the generation never leads outside it.
	escape := makeGen(t, map[string]string{"usr/bin": "->" + t.TempDir()}, nil)
	if ok, _ := linkFleet(escape); ok {
		t.Fatal("linked through a symlink out of the generation")
	}
}
