package podseed

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An export is what the seed's extraction reads back: the same tree, with
// modes, links and content, its summary naming the stream it wrote.
func TestExportWorkspaceRoundTrips(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ws")
	write := func(rel, body string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".devcontainer/devcontainer.json", `{"image":"x"}`, 0o644)
	write("bin/run.sh", "#!/bin/sh\n", 0o755)
	write("node_modules/dep/index.js", "volume content", 0o644)
	if err := os.Symlink("bin/run.sh", filepath.Join(src, "run")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	sum, err := ExportWorkspace(&buf, src, ExportOptions{Mounts: []string{filepath.Join(src, "node_modules")}})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(buf.Bytes())
	if sum.Size != int64(buf.Len()) || sum.SHA256 != hex.EncodeToString(h[:]) {
		t.Errorf("summary size %d sha %s, stream %d bytes sha %x", sum.Size, sum.SHA256, buf.Len(), h)
	}
	if sum.Files != 2 || sum.Links != 1 || sum.Bytes != int64(len(`{"image":"x"}`)+len("#!/bin/sh\n")) {
		t.Errorf("summary %+v", sum)
	}
	if len(sum.Mounts) != 1 || sum.Mounts[0] != "node_modules" {
		t.Errorf("mounts %v, want node_modules sent empty", sum.Mounts)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if _, err := ExtractWorkspace(bytes.NewReader(buf.Bytes()), dst, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, ".devcontainer/devcontainer.json")); string(got) != `{"image":"x"}` {
		t.Errorf("devcontainer.json %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dst, "bin/run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh: %v %v", fi, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "run")); err != nil || link != "bin/run.sh" {
		t.Errorf("run link %q %v", link, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("empty dir: %v %v", fi, err)
	}
	entries, err := os.ReadDir(filepath.Join(dst, "node_modules"))
	if err != nil || len(entries) != 0 {
		t.Errorf("the mount came back with %v (%v), want an empty dir", entries, err)
	}
}

// What the exporter may not read is left out and named, never a failure: a
// user's own 0600 file of another owner must not stop a rebuild.
func TestExportWorkspaceLeavesOutWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "secret"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(src, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var buf bytes.Buffer
	sum, err := ExportWorkspace(&buf, src, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 1 || sum.Skipped != 2 || len(sum.Unreadable) != 2 {
		t.Fatalf("summary %+v, want ok.txt sent and secret + locked/ named", sum)
	}
	names := tarNames(t, buf.Bytes())
	if !names["./ok.txt"] || names["./secret"] {
		t.Errorf("entries %v", names)
	}
	for _, u := range sum.Unreadable {
		if !strings.Contains(u, "permission denied") {
			t.Errorf("unreadable %q does not say why", u)
		}
	}
}

// A file that shrinks while it is sent keeps the size its header promised:
// the stream stays a valid tar, the missing tail zeros.
func TestExportWorkspaceFileShrinksMidway(t *testing.T) {
	p := filepath.Join(t.TempDir(), "shrinks")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Stat said 1000 bytes; only 10 are left by the time they are read.
	n, err := writeSized(tw, "shrinks", info, strings.NewReader(strings.Repeat("a", 10)))
	if err != nil || n != 1000 {
		t.Fatalf("writeSized = %d, %v", n, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	h, err := tr.Next()
	if err != nil || h.Size != 1000 {
		t.Fatalf("header %+v %v", h, err)
	}
	body, _ := io.ReadAll(tr)
	if want := strings.Repeat("a", 10) + strings.Repeat("\x00", 990); string(body) != want {
		t.Errorf("body is %d bytes, not the 10 left padded to 1000", len(body))
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the entry: %v, want the end", err)
	}
}

// A receiver that goes away fails the export.
func TestExportWorkspaceReportsAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big"), bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	left := 4096
	_, err := ExportWorkspace(failAfter{n: &left}, dir, ExportOptions{})
	if err == nil {
		t.Fatal("an export into a closed pipe succeeded")
	}
}

// failAfter takes n bytes, then fails every write.
type failAfter struct{ n *int }

func (f failAfter) Write(p []byte) (int, error) {
	if *f.n <= 0 {
		return 0, errors.New("pipe closed")
	}
	k := min(len(p), *f.n)
	*f.n -= k
	return k, nil
}

func TestParseExportSummary(t *testing.T) {
	want := ExportSummary{Files: 3, Dirs: 2, Bytes: 9, Size: 4096, SHA256: "ab", Unreadable: []string{"x: permission denied"}}
	stderr := "fleet pod-job ws-export: reading the mounts: nope\n" + FormatExportSummary(want) + "\n"
	got, ok := ParseExportSummary(stderr)
	if !ok || got.Files != 3 || got.SHA256 != "ab" || len(got.Unreadable) != 1 {
		t.Fatalf("ParseExportSummary = %+v, %v", got, ok)
	}
	if _, ok := ParseExportSummary("tar: something broke\n"); ok {
		t.Error("a summary from an export that never ended")
	}
}

func tarNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = true
	}
}
