package platform

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// open.go hands a URL or a local file to the operating system's default handler
// — the thing a desktop does on double-click. It is shared by the TUI (opening
// a PR link, or a file delivered by an in-instance `fleet open`) and the host
// CLI (`fleet open`), so every "open this on the user's machine" path picks the
// same per-OS opener.

// EnvOpener names the program `fleet open` uses instead of the platform default
// (xdg-open / open / wslview). It is invoked with the file's path appended to
// its (whitespace-separated) arguments — e.g. FLEET_OPENER="imv -f" — and is
// how a headless or unusual desktop can still route opens somewhere useful. It
// applies to FILE opens only; URLs always go to the platform browser opener.
const EnvOpener = "FLEET_OPENER"

// openGrace is how long OpenFile waits for the opener to exit before deciding
// it is hosting the viewer itself. Desktop openers (gio/kde-open, macOS open,
// wslview) hand off and return within milliseconds; a failure (no handler, no
// display) also surfaces well inside this window. Only a bare xdg-open with no
// desktop session runs the application in the foreground, and that must not
// pin the caller for the viewer's whole lifetime.
const openGrace = 2 * time.Second

// stderrCap bounds how much of the opener's stderr is kept for the error
// message. The reaper keeps draining past it (the pipe must not fill up while
// a long-lived viewer runs), it just stops remembering.
const stderrCap = 2048

// OpenCommand builds the per-OS "open with the default handler" command for
// target, a URL or a local path. WSL is special cased (xdg-open there opens
// nothing useful) to reach the Windows handler. Every opener receives target
// as a separate, non-shell-interpreted argument; the WSL PowerShell fallback
// passes it via the environment rather than interpolating it into the -Command
// string, so a target containing PowerShell metacharacters can't inject code
// on the host.
func OpenCommand(target string) (*exec.Cmd, error) {
	switch {
	case runtime.GOOS == "darwin":
		return exec.Command("open", target), nil
	case runtime.GOOS == "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target), nil
	case IsWSL():
		// wslu's wslview is the clean path; fall back to the Windows shell's
		// start handler via powershell when it isn't installed.
		if _, err := exec.LookPath("wslview"); err == nil {
			return exec.Command("wslview", target), nil
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Process -FilePath $env:FLEET_OPEN_TARGET")
		cmd.Env = append(os.Environ(), "FLEET_OPEN_TARGET="+target)
		return cmd, nil
	case runtime.GOOS == "linux":
		return exec.Command("xdg-open", target), nil
	default:
		return nil, fmt.Errorf("don't know how to open %q on %s", target, runtime.GOOS)
	}
}

// OpenFileCommand builds the command that opens the local file at path with the
// user's default application: the FLEET_OPENER override when set, otherwise the
// platform opener. On WSL without wslview the PowerShell fallback needs a
// Windows path, so the Linux path is translated with wslpath first.
func OpenFileCommand(path string) (*exec.Cmd, error) {
	if opener := strings.Fields(os.Getenv(EnvOpener)); len(opener) > 0 {
		return exec.Command(opener[0], append(opener[1:], path)...), nil
	}
	if IsWSL() {
		if _, err := exec.LookPath("wslview"); err != nil {
			if win, err := exec.Command("wslpath", "-w", path).Output(); err == nil {
				path = strings.TrimSpace(string(win))
			}
		}
	}
	return OpenCommand(path)
}

// OpenFile opens the local file at path with the user's default application and
// reports an opener that failed outright (no handler, no display, unknown
// program). It returns once the opener has handed off — or after openGrace if
// the opener is itself hosting the application — without ever waiting for the
// viewer to close; the process is reaped in the background either way.
func OpenFile(path string) error {
	cmd, err := OpenFileCommand(path)
	if err != nil {
		return err
	}
	return runDetached(cmd, openGrace)
}

// runDetached starts cmd with no terminal attached, waits up to grace for it to
// exit, and reports its exit error if it did. A process still running after
// grace is left alone (nil) and reaped by a background goroutine, so a caller
// on a UI thread or at a shell prompt is never held for the child's lifetime.
func runDetached(cmd *exec.Cmd, grace time.Duration) error {
	stderr := &cappedBuffer{limit: stderrCap}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("%s: %w: %s", cmd.Args[0], err, msg)
			}
			return fmt.Errorf("%s: %w", cmd.Args[0], err)
		}
		return nil
	case <-time.After(grace):
		return nil
	}
}

// cappedBuffer is an io.Writer that remembers at most limit bytes and silently
// discards the rest, so a chatty long-lived child can't grow it without bound
// while still being drained.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
