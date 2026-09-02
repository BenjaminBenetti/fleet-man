package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// open.go hands a URL or a local file to the operating system's default handler
// — the thing a desktop does on double-click. It is shared by the TUI (opening
// a PR link, or a file delivered by an in-instance `fleet open`) and the host
// CLI (`fleet open`), so every "open this on the user's machine" path picks the
// same per-OS opener.

// EnvOpener names the program `fleet open` uses instead of the platform default
// (xdg-open / open / wslview). Its value is split on whitespace into a program
// and arguments (so a program path containing a space cannot be expressed) and
// the file's path is appended last — e.g. FLEET_OPENER="imv -f". It is how a
// headless or unusual desktop can still route opens somewhere useful. It
// applies to FILE opens only; URLs always go to the platform browser opener.
const EnvOpener = "FLEET_OPENER"

// openGrace is how long OpenFile waits for the opener to exit before deciding
// it is hosting the viewer itself. Desktop openers (gio/kde-open, macOS open,
// wslview) hand off and return within milliseconds; a failure (no handler, no
// display) also surfaces well inside this window. Only a bare xdg-open with no
// desktop session runs the application in the foreground, and that must not
// pin the caller for the viewer's whole lifetime.
const openGrace = 2 * time.Second

// stderrCap bounds how much of the opener's stderr is quoted in an error.
const stderrCap = 2048

// launcherExtensions are file types every platform opener LAUNCHES rather than
// views: rundll32's FileProtocolHandler runs .exe/.bat/.js/.vbs/.lnk, macOS
// `open` launches .app/.command/.pkg, and the common Linux desktops execute an
// executable .desktop handed to xdg-open. `fleet open` exists for images,
// charts and PDFs; refusing these costs nothing and keeps "an untrusted
// container asked the human to open a file" a non-executing action.
var launcherExtensions = map[string]bool{
	".exe": true, ".bat": true, ".cmd": true, ".com": true, ".scr": true, ".pif": true,
	".msi": true, ".msp": true, ".reg": true, ".lnk": true, ".hta": true,
	".js": true, ".jse": true, ".vbs": true, ".vbe": true, ".wsf": true, ".wsh": true, ".ps1": true,
	".desktop": true, ".command": true, ".tool": true, ".app": true, ".pkg": true,
	".sh": true, ".bash": true, ".zsh": true, ".run": true, ".appimage": true,
}

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
// returns the absolute path it handed over (the 1-arg `fleet open` lands a bare
// name in the cwd; absolute is unambiguous whatever the handler's own cwd is).
// It refuses to open anything the platform opener would LAUNCH rather than view
// — an executable file, or one of launcherExtensions — since the file came out
// of a container. It reports an opener that failed outright (no handler, no
// display, unknown program) and returns once the opener has handed off — or
// after openGrace if the opener is itself hosting the application — without
// ever waiting for the viewer to close; the process is reaped in the background
// either way.
func OpenFile(path string) (string, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if launcher, err := IsLauncherFile(path); err != nil {
		return path, err
	} else if launcher {
		return path, fmt.Errorf("refusing to open an executable; it was copied to %s", path)
	}
	cmd, err := OpenFileCommand(path)
	if err != nil {
		return path, err
	}
	return path, runDetached(cmd, openGrace)
}

// IsLauncherFile reports whether the file at path is something an opener would
// launch rather than view: a regular file with an exec bit, or any name (file
// or directory — macOS .app bundles are directories) with a launcherExtensions
// suffix. A directory without such a suffix is fine: openers show it in the
// file manager.
func IsLauncherFile(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if launcherExtensions[strings.ToLower(filepath.Ext(path))] {
		return true, nil
	}
	return fi.Mode().IsRegular() && fi.Mode()&0o111 != 0, nil
}

// runDetached starts cmd with no terminal attached, waits up to grace for it to
// exit, and reports its exit error (with its stderr) if it did. A process still
// running after grace is left alone (nil) and reaped by a background goroutine,
// so a caller on a UI thread or at a shell prompt is never held for the child's
// lifetime.
//
// Stderr goes to a temp FILE, not a pipe: the host CLI exits right after the
// grace, and a pipe whose reader is gone would SIGPIPE the viewer on its next
// warning (GTK/Qt print plenty); a file never does. The child is also put in
// its own session (unix) so closing the terminal doesn't SIGHUP it. The file is
// unlinked as soon as it has been read or the child outlived the grace — an
// open descriptor keeps working on an unlinked file.
func runDetached(cmd *exec.Cmd, grace time.Duration) error {
	stderr, err := os.CreateTemp("", "fleet-open-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = stderr
	detachFromTerminal(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if msg := readHead(stderr.Name(), stderrCap); msg != "" {
				return fmt.Errorf("%s: %w: %s", cmd.Args[0], err, msg)
			}
			return fmt.Errorf("%s: %w", cmd.Args[0], err)
		}
		return nil
	case <-time.After(grace):
		return nil
	}
}

// readHead returns the first n bytes of the file at path, trimmed; empty when
// unreadable.
func readHead(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:read]))
}
