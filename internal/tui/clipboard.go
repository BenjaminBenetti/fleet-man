package tui

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/platform"
)

// ===========================================
// Clipboard Sync
// ===========================================

// clipboardSync polls the outer tmux paste buffer and copies changes
// to the system clipboard. This provides clipboard support for ALL
// terminal emulators — including those without OSC 52 (e.g. GNOME
// Terminal, Ptyxis). For terminals that DO support OSC 52 (e.g.
// Alacritty, Kitty), the goroutine is harmless since it re-copies
// the same content that OSC 52 already placed on the clipboard.
type clipboardSync struct {
	clipCmds    [][]string
	lastBuffers []clipboardBuffer // last successful write to each destination
}

// tmux gives each copy an automatic buffer name, including repeated copies of
// identical text. Keep the contents too so updates to an existing named buffer
// still propagate.
type clipboardBuffer struct {
	name    string
	content string
}

// Bound each subprocess separately so a stuck PRIMARY writer cannot permanently
// stop regular clipboard updates. WaitDelay also bounds pipe I/O if a child of
// the command inherits a pipe and keeps it open after the command exits.
const clipboardCommandTimeout = 5 * time.Second
const clipboardWaitDelay = 100 * time.Millisecond

// clipboardCmds returns the system clipboard commands for the current
// platform and display server. On Linux, two commands are returned so
// the content is written to both the regular clipboard (Ctrl+V) and
// the primary selection (middle-click). On macOS, only pbcopy is
// returned because there is no primary selection concept.
func clipboardCmds() [][]string {
	return clipboardCmdsFor(runtime.GOOS, os.Getenv("WAYLAND_DISPLAY") != "", platform.IsWSL())
}

func clipboardCmdsFor(goos string, hasWayland, isWSL bool) [][]string {
	if goos == "darwin" {
		return [][]string{{"pbcopy"}}
	}
	if goos == "linux" && isWSL {
		return [][]string{{"powershell.exe", "-NoProfile", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())"}}
	}
	if hasWayland {
		return [][]string{{"wl-copy"}, {"wl-copy", "--primary"}}
	}
	return [][]string{{"xclip", "-sel", "clip", "-i"}, {"xclip", "-sel", "primary", "-i"}}
}

// newClipboardSync creates a clipboard synchroniser. Returns nil if
// tmux is unavailable or no system clipboard tool can be found.
func newClipboardSync() *clipboardSync {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil
	}
	commands := clipboardCmds()
	if _, err := exec.LookPath(commands[0][0]); err != nil {
		return nil
	}
	return &clipboardSync{clipCmds: commands}
}

// Start begins polling the tmux paste buffer and synchronising
// changes to the system clipboard. Blocks until ctx is cancelled;
// call from a goroutine.
func (cs *clipboardSync) Start(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cs.poll(ctx)
		}
	}
}

func clipboardTmuxOutput(ctx context.Context, args ...string) ([]byte, error) {
	readCtx, cancel := context.WithTimeout(ctx, clipboardCommandTimeout)
	defer cancel()
	readCmd := exec.CommandContext(readCtx, "tmux", args...)
	readCmd.WaitDelay = clipboardWaitDelay
	return readCmd.Output()
}

// poll detects a new tmux copy using both its buffer name and contents, then
// updates each clipboard destination that has not yet received that copy.
func (cs *clipboardSync) poll(ctx context.Context) {
	out, err := clipboardTmuxOutput(ctx, "display-message", "-p", "#{buffer_name}")
	if err != nil {
		return
	}
	name := strings.TrimSuffix(string(out), "\n")
	// __fleet_primary is the temporary buffer used by the host middle-click
	// binding. Pasting must not count as a new copy or change either clipboard.
	if name == "" || name == "__fleet_primary" {
		return
	}
	// Read this specific buffer so a selection made between the two queries
	// cannot pair one copy's name with another's text. If the buffer was evicted
	// meanwhile, the next poll picks up the latest copy.
	out, err = clipboardTmuxOutput(ctx, "show-buffer", "-b", name)
	if err != nil || len(out) == 0 {
		return
	}
	buf := clipboardBuffer{name: name, content: string(out)}
	if len(cs.lastBuffers) != len(cs.clipCmds) {
		cs.lastBuffers = make([]clipboardBuffer, len(cs.clipCmds))
	}

	for i, clipCmd := range cs.clipCmds {
		if ctx.Err() != nil {
			return
		}
		if buf == cs.lastBuffers[i] {
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, clipboardCommandTimeout)
		// Invoke the tool directly: cancelling a shell pipeline only kills the
		// shell, leaving its clipboard process running. Stdin also avoids the
		// argument-size limit when copying large selections.
		cmd := exec.CommandContext(writeCtx, clipCmd[0], clipCmd[1:]...)
		cmd.Stdin = strings.NewReader(buf.content)
		cmd.WaitDelay = clipboardWaitDelay
		err := cmd.Run()
		cancel()
		if err == nil {
			cs.lastBuffers[i] = buf
		}
	}
}
