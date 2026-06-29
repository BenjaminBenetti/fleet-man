package coder

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteStageNameUnique confirms the remote scp staging name lives in the
// fixed metacharacter-free /tmp dir (so the unquoted scp operand is transport-
// safe regardless of the destination), differs across calls (so concurrent
// copies don't collide), and carries no shell metacharacters.
func TestRemoteStageNameUnique(t *testing.T) {
	a, b := remoteStageName(), remoteStageName()
	if a == b {
		t.Fatalf("stage names collide: %q", a)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, coderStageDir+"/.fleet-scp.") {
			t.Errorf("stage name %q is not in the fixed staging dir %q", n, coderStageDir)
		}
		if strings.ContainsAny(n, " \t'\"$`\\*?(){}[]") {
			t.Errorf("stage name %q contains a shell metacharacter (unsafe as an unquoted scp operand)", n)
		}
	}
}

// TestLocalFileForCopyOsFile confirms an *os.File source is scp'd directly (its
// path reused, no buffering copy) so staging the already-on-disk fleet binary
// doesn't pay an extra full-size temp write.
func TestLocalFileForCopyOsFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(src, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	path, cleanup, err := localFileForCopy(f)
	if err != nil {
		t.Fatalf("localFileForCopy: %v", err)
	}
	defer cleanup()
	if path != src {
		t.Fatalf("path = %q, want the source path %q (no buffering)", path, src)
	}
	// Cleanup for the direct path is a no-op: the source must survive it.
	cleanup()
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("direct-path cleanup removed the source: %v", err)
	}
}

// TestLocalFileForCopyReader confirms a non-file reader is buffered to a host
// temp whose bytes match, and that cleanup removes it.
func TestLocalFileForCopyReader(t *testing.T) {
	content := []byte("buffer me\x00please")
	path, cleanup, err := localFileForCopy(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("localFileForCopy: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("buffered content = %q, want %q", got, content)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove temp %q (err %v)", path, err)
	}
}
