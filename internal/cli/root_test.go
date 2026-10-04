package cli

import (
	"strings"
	"testing"
)

func TestPasteClipboardCommandWSL(t *testing.T) {
	got := pasteClipboardCommandFor(true)
	if !strings.Contains(got, "powershell.exe") || !strings.Contains(got, "Get-Clipboard") {
		t.Fatalf("WSL paste command = %q, want PowerShell Get-Clipboard", got)
	}
}

func TestPasteClipboardCommandLinux(t *testing.T) {
	got := pasteClipboardCommandFor(false)
	for _, want := range []string{"wl-paste", "xsel", "xclip", "pbpaste"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Linux paste command = %q, want %q fallback", got, want)
		}
	}
}

// tmuxCommands splits a tmux ";"-chained argv into its commands.
func tmuxCommands(args []string) [][]string {
	var cmds [][]string
	cur := []string{}
	for _, a := range args[1:] {
		if a == ";" {
			cmds = append(cmds, cur)
			cur = []string{}
			continue
		}
		cur = append(cur, a)
	}
	return append(cmds, cur)
}

func TestTmuxSetupArgsHidesStatusForFleetSessionOnly(t *testing.T) {
	cmds := tmuxCommands(tmuxSetupArgs("fleet-abc123", "/bin/fleet", true))
	found := false
	for _, c := range cmds {
		got := strings.Join(c, " ")
		if got == "set -t fleet-abc123 status off" {
			found = true
		}
		// A global status change would hide the bar on every session of
		// a user's shared tmux server.
		if c[0] == "set" && strings.Contains(c[1], "g") && c[len(c)-2] == "status" {
			t.Fatalf("status set globally: %q", got)
		}
	}
	if !found {
		t.Fatalf("no session-scoped status off in %q", cmds)
	}
}

func TestTmuxSetupArgsTerminalFeaturesLast(t *testing.T) {
	for _, vimKeys := range []bool{true, false} {
		cmds := tmuxCommands(tmuxSetupArgs("fleet-abc123", "/bin/fleet", vimKeys))
		if last := strings.Join(cmds[len(cmds)-1], " "); !strings.HasPrefix(last, "set -as terminal-features") {
			t.Fatalf("vimKeys=%v: last command = %q, want terminal-features (fails on tmux <3.2)", vimKeys, last)
		}
	}
}
