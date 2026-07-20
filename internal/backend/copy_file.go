package backend

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// copy_file.go holds the transport-agnostic helpers behind the Backend.CopyFile
// strategy and the small-file inline writer. They exist because a remote command
// that reads stdin until EOF (e.g. `cat > file`) hangs forever on transports
// that do not half-close a remote command's stdin — notably `coder ssh`, whose
// agent SSH server never delivers the stdin EOF. Two stdin-EOF-free techniques
// live here:
//
//   - WriteFileInline / InlineWriteScript embed the payload in the command as
//     base64 (decoded in-container), so no stdin is involved at all. Use it for
//     SMALL files; the whole command line is bounded by the host ARG_MAX.
//
//   - StreamWriteScript is the classic stdin-streamed write (`cat > tmp`) used by
//     backends whose transport DOES deliver stdin EOF (devcontainer's local
//     docker exec, codespaces' OpenSSH). The coder backend cannot use it and
//     ships its own scp-based CopyFile instead.

// CopyTimeout bounds a single CopyFile transport so a wedged copy can never stall
// instance provisioning (which is otherwise best-effort but has no deadline of
// its own). Generous enough for a multi-tens-of-MB binary over a slow tunnel.
const CopyTimeout = 5 * time.Minute

// maxInlineRaw caps the raw payload an inline (base64-in-argv) write accepts.
// The base64 expansion (+~33%) plus the surrounding script must fit comfortably
// under the host ARG_MAX (as low as 128KB on some systems), so the cap is kept
// well below that. Every inline caller (fleet.rc, agent hook scripts and
// settings.json) is a few KB, so this is never hit in practice; it exists to
// fail loudly rather than silently exceed ARG_MAX if a payload ever grows.
const maxInlineRaw = 64 * 1024

// ShellQuote wraps value in single quotes so any path is safe to drop into a
// /bin/sh command literal. Thin delegate kept for the established name; the
// one implementation lives in internal/shellquote.
func ShellQuote(value string) string {
	return shellquote.Single(value)
}

// InlineWriteScript returns a /bin/sh command (as argv: {"sh","-c",body}) that
// writes content to remotePath with the given mode, WITHOUT reading stdin: the
// payload travels base64-encoded inside the command itself and is decoded in the
// container. The write is atomic (same-dir temp + rename) and creates the parent
// directory. Returns an error when content exceeds maxInlineRaw — callers with
// large payloads must use the streaming Backend.CopyFile instead.
//
// `printf %s '<b64>'` is a shell builtin, so the base64 blob never becomes its
// own argv entry (no extra ARG_MAX pressure beyond the single `sh -c` literal),
// and base64's alphabet contains no single quote, so the blob is safe to embed
// single-quoted verbatim.
func InlineWriteScript(remotePath string, content []byte, mode int) ([]string, error) {
	if len(content) > maxInlineRaw {
		return nil, fmt.Errorf("inline write of %q: payload %d bytes exceeds inline cap %d (use CopyFile)", remotePath, len(content), maxInlineRaw)
	}
	b64 := base64.StdEncoding.EncodeToString(content)
	qPath := ShellQuote(remotePath)
	// $$ (the sh PID) keeps the temp unique against a concurrent writer in the
	// same directory; set -e aborts before the rename if any step fails, and the
	// EXIT trap removes the temp on that abort so a failed write leaves no orphan
	// in the dest dir. On success the temp is already renamed away, so the trap's
	// rm is a harmless no-op.
	body := fmt.Sprintf(
		`set -e; d=$(dirname %[1]s); mkdir -p "$d"; t="$d/.fleet-inline.$$"; trap 'rm -f "$t"' EXIT; printf %%s '%[2]s' | base64 -d > "$t"; chmod %[3]o "$t"; mv -f "$t" %[1]s`,
		qPath, b64, mode,
	)
	return []string{"sh", "-c", body}, nil
}

// WriteFileInline runs InlineWriteScript against the backend's exec surface with
// a deadline, returning a wrapped error (including any container stderr) on
// failure. It is the one-call front door for staging a small in-memory file into
// an instance in a way that works on every backend, including coder.
func WriteFileInline(b Backend, workspaceDir, remotePath string, content []byte, mode int) error {
	argv, err := InlineWriteScript(remotePath, content, mode)
	if err != nil {
		return err
	}
	out, err := b.ExecCommand(workspaceDir, argv).CombinedOutputWithTimeout(CopyTimeout)
	if err != nil {
		return fmt.Errorf("inline write %q: %w (%s)", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StreamWriteScript returns a /bin/sh command (as argv: {"sh","-c",body}) that
// reads file content from STDIN and writes it to remotePath with the given mode.
// The write is atomic (same-dir temp + rename) and creates the parent directory.
//
// This is the stdin-streamed counterpart used by CopyFile on backends whose
// transport delivers stdin EOF. It must NOT be used on the coder backend, whose
// `cat` would block forever waiting for an EOF that never arrives.
func StreamWriteScript(remotePath string, mode int) []string {
	qPath := ShellQuote(remotePath)
	// The EXIT trap removes the temp if any step fails before the rename (set -e),
	// so an interrupted stream leaves no orphan .fleet-copy.$$ in the dest dir; on
	// success the temp is already renamed away and the rm is a no-op.
	body := fmt.Sprintf(
		`set -e; d=$(dirname %[1]s); mkdir -p "$d"; t="$d/.fleet-copy.$$"; trap 'rm -f "$t"' EXIT; cat > "$t"; chmod %[2]o "$t"; mv -f "$t" %[1]s`,
		qPath, mode,
	)
	return []string{"sh", "-c", body}
}
