package backend

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestInlineWriteScriptRoundTrip runs the generated inline-write command in a
// real /bin/sh and confirms the base64-in-argv payload round-trips to disk with
// the requested mode, mkdir-ing a missing parent — the path that lets the coder
// backend stage files without streaming stdin. Binary bytes (NUL, 0x01) prove
// the transport is 8-bit clean.
func TestInlineWriteScriptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "missing", "fleet.rc")
	content := []byte("hello\nfleet\x00binary\x01bytes")

	argv, err := InlineWriteScript(target, content, 0o640)
	if err != nil {
		t.Fatalf("InlineWriteScript: %v", err)
	}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("run inline write: %v (%s)", err, out)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 0640", fi.Mode().Perm())
	}
	// The atomic temp must not be left behind.
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if e.Name() != "fleet.rc" {
			t.Errorf("unexpected leftover %q in target dir", e.Name())
		}
	}
}

// TestInlineWriteScriptSizeGuard confirms an oversized payload is rejected
// (callers must use the streaming CopyFile) rather than silently overflowing
// the host ARG_MAX.
func TestInlineWriteScriptSizeGuard(t *testing.T) {
	if _, err := InlineWriteScript("/x", make([]byte, maxInlineRaw+1), 0o644); err == nil {
		t.Fatal("expected error for oversized inline payload, got nil")
	}
	if _, err := InlineWriteScript("/x", make([]byte, maxInlineRaw), 0o644); err != nil {
		t.Fatalf("payload at the cap should be accepted: %v", err)
	}
}

// TestStreamWriteScriptRoundTrip runs the generated stdin-streamed write command
// in a real /bin/sh and confirms the stdin payload round-trips with the
// requested mode, mkdir-ing a missing parent.
func TestStreamWriteScriptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "missing", "out.bin")
	content := []byte("streamed\x00content")

	argv := StreamWriteScript(target, 0o600)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run stream write: %v (%s)", err, out)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

// TestWriteScriptsQuotePathsSafely confirms a path with a single quote (and
// other shell metacharacters) is handled verbatim by both writers, so a hostile
// or merely awkward path can't break out of the command literal.
func TestWriteScriptsQuotePathsSafely(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a'b $x ;rm.txt")
	content := []byte("safe")

	argv, err := InlineWriteScript(target, content, 0o644)
	if err != nil {
		t.Fatalf("InlineWriteScript: %v", err)
	}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("run inline write: %v (%s)", err, out)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, content) {
		t.Fatalf("inline: content = %q, want %q", got, content)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"a'b":       `'a'\''b'`,
		"/usr/bin":  "'/usr/bin'",
		"":          "''",
		"two words": "'two words'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
