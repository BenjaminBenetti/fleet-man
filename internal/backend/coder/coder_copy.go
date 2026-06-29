package coder

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// coderPrepTimeout bounds the small stdin-free helper execs (mkdir, chmod+mv,
// cleanup) that bracket the scp transfer. They read no stdin, so they cannot hit
// the coder stdin-EOF hang; the deadline only guards against a stuck tunnel.
const coderPrepTimeout = 30 * time.Second

// coderCopySeq makes the remote staging name unique within this process so two
// concurrent CopyFile calls never collide on it.
var coderCopySeq atomic.Uint64

// coderStageDir is the fixed directory CopyFile scp's into before the install
// rename. It is deliberately NOT a sibling of remotePath: the scp remote operand
// is the one path in this file that cannot be ShellQuote'd (see the scp call),
// and remotePath's parent can legitimately contain spaces — a `fleet copy` dest
// directory is only `test -d`-probed, not character-restricted. Staging instead
// in /tmp, whose name carries no shell metacharacters, keeps that operand safe
// under both scp transports without quoting. /tmp is writable on every backend
// (the binary and automation-event staging paths already rely on it).
const coderStageDir = "/tmp"

// remoteStageName returns a hidden, per-call-unique path in coderStageDir to scp
// into before the install rename — the out-of-band analogue of the in-shell
// `$$`-suffixed temp the stdin-streaming backends use. Its characters are
// restricted to [0-9a-f.] so the unquoted scp operand is transport-safe.
func remoteStageName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/.fleet-scp.%d.%x", coderStageDir, coderCopySeq.Add(1), b)
}

// CopyFile transfers src INTO the coder workspace (its nested devcontainer) at
// remotePath. It cannot stream over `coder ssh` stdin like the other backends:
// the coder agent's SSH server (gliderlabs/ssh) never half-closes a remote
// command's stdin, so a `cat > file` reading to EOF hangs forever (issue #223).
//
// Instead it uses scp, whose length-framed protocol needs no stdin EOF,
// tunnelled over `coder ssh --stdio` as an OpenSSH ProxyCommand — the transport
// coder documents for scp/rsync, which lands the file inside the nested
// devcontainer agent. Sequence:
//
//  1. materialise src to a host file (an *os.File source is used directly);
//  2. mkdir -p the destination's parent (scp will not create it);
//  3. scp the host file to a fixed metacharacter-free temp in /tmp;
//  4. chmod + rename it into place via a normal exec (stdin-free, hang-proof).
//
// The rename in step 4 (not the scp in step 3) targets remotePath, so a `fleet
// copy` destination that contains spaces is handled by the ShellQuote'd `mv` —
// the scp operand never sees it. Because the temp lives in /tmp rather than
// beside remotePath, that `mv` may cross filesystems (a copy+unlink, not an
// atomic rename) when remotePath is outside /tmp; that is acceptable for the
// best-effort staging this serves and matches no concurrent-reader expectation.
//
// scp defaults to the SFTP subsystem, which the coder agent provides; if a
// particular template's nested agent lacks it the transfer fails cleanly (the
// caller treats binary staging as best-effort) rather than hanging.
//
// Requirements & limitations: `scp` must be on the host PATH and be an
// OpenSSH-compatible build (the `-o ProxyCommand` syntax is OpenSSH-specific;
// e.g. Dropbear's scp would reject it). The scp deadline is the standalone
// CopyTimeout rather than a caller context — Backend.CopyFile takes no context,
// so a client that aborts an in-flight `fleet copy` cannot cancel the scp before
// that timeout; the bound keeps an abandoned copy from leaking indefinitely.
//
// remotePath should be ABSOLUTE. Every caller in the staging path passes one
// (the binary stages to /tmp, the automation event file to /tmp). A relative
// remotePath resolves against scp's / `coder ssh`'s login dir (home) on coder —
// which differs from the workspace folder a devcontainer/codespaces write would
// use, and from the path the `fleet copy` handler reports — so a `fleet copy`
// with an empty/relative dest lands in an undefined spot on coder. That path is
// part of the still-to-be-validated coder `fleet copy` support; the #223 fix
// (binary + automation staging) is unaffected because it is always absolute.
func (coderBackend *CoderBackend) CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error {
	if _, err := exec.LookPath("scp"); err != nil {
		return fmt.Errorf("copy into %q: scp not found on host (required for coder file transfer): %w", remotePath, err)
	}

	localPath, cleanup, err := localFileForCopy(src)
	if err != nil {
		return fmt.Errorf("copy into %q: stage source: %w", remotePath, err)
	}
	defer cleanup()

	name := coderWorkspaceName(workspaceDir)
	target := coderBackend.resolveSSHTarget(name)
	remoteTmp := remoteStageName()

	// scp cannot create the destination directory; make sure it exists first.
	mkdir := fmt.Sprintf(`mkdir -p "$(dirname %s)"`, backend.ShellQuote(remotePath))
	if out, err := coderBackend.ExecCommand(workspaceDir, []string{"sh", "-c", mkdir}).CombinedOutputWithTimeout(coderPrepTimeout); err != nil {
		return fmt.Errorf("copy into %q: prepare dir: %w (%s)", remotePath, err, strings.TrimSpace(string(out)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), backend.CopyTimeout)
	defer cancel()
	// The remote operand (target:remoteTmp) is intentionally NOT shell-quoted,
	// unlike the mkdir/install/cleanup execs around it. The two scp transports
	// disagree on how to read it: the default SFTP transfer (OpenSSH 9.0+) takes
	// the post-colon path LITERALLY, so a `'..'` wrapper would become part of the
	// filename, while the legacy pre-9.0 protocol shell-expands it (where quoting
	// would be required). No single quoting choice is right for both — so instead
	// remoteTmp is constructed to need none: it lives in /tmp with only [0-9a-f.]
	// characters (see remoteStageName), which is safe verbatim under either
	// transport. The spacey final path is reached by the ShellQuote'd `mv` below,
	// never by scp.
	scp := exec.CommandContext(ctx, "scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=coder ssh --stdio %h",
		localPath, target+":"+remoteTmp,
	)
	if out, err := scp.CombinedOutput(); err != nil {
		// scp may have written a partial remote temp before failing.
		coderBackend.removeRemote(workspaceDir, remoteTmp)
		return fmt.Errorf("copy into %q: scp: %w (%s)", remotePath, err, strings.TrimSpace(string(out)))
	}

	// chmod to the requested mode then atomically rename into place. Neither
	// command reads stdin, so they are immune to the coder stdin-EOF hang.
	install := fmt.Sprintf(`set -e; chmod %o %[2]s; mv -f %[2]s %[3]s`,
		mode, backend.ShellQuote(remoteTmp), backend.ShellQuote(remotePath))
	if out, err := coderBackend.ExecCommand(workspaceDir, []string{"sh", "-c", install}).CombinedOutputWithTimeout(coderPrepTimeout); err != nil {
		// The rename did not complete; don't leave the staged temp behind.
		coderBackend.removeRemote(workspaceDir, remoteTmp)
		return fmt.Errorf("copy into %q: install: %w (%s)", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeRemote best-effort deletes a leftover staging file inside the workspace
// after a failed transfer, so repeated failures don't accumulate orphans.
func (coderBackend *CoderBackend) removeRemote(workspaceDir, path string) {
	rm := fmt.Sprintf(`rm -f %s`, backend.ShellQuote(path))
	_, _ = coderBackend.ExecCommand(workspaceDir, []string{"sh", "-c", rm}).CombinedOutputWithTimeout(coderPrepTimeout)
}

// localFileForCopy yields a host filesystem path for src that scp can read. When
// src is already an *os.File (e.g. the open fleet binary) its path is used
// directly with a no-op cleanup; any other reader is buffered into a temp file
// the caller must remove via the returned cleanup.
func localFileForCopy(src io.Reader) (path string, cleanup func(), err error) {
	noop := func() {}
	if f, ok := src.(*os.File); ok && f.Name() != "" {
		return f.Name(), noop, nil
	}
	tmp, err := os.CreateTemp("", "fleet-copy-*")
	if err != nil {
		return "", noop, err
	}
	remove := func() { _ = os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		remove()
		return "", noop, err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return "", noop, err
	}
	return tmp.Name(), remove, nil
}
