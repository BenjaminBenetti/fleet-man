package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestShQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "'hello'"},
		{"with spaces", "hello world", "'hello world'"},
		{"with single quote", "it's", `'it'\''s'`},
		{"empty", "", "''"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shQuote(tt.in)
			if got != tt.want {
				t.Fatalf("shQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeSessionName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "agent-1", "agent-1"},
		{"with dot", "my.instance", "my-instance"},
		{"with colon", "foo:bar", "foo-bar"},
		{"mixed", "a.b:c", "a-b-c"},
		{"empty", "", "fleet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeSessionName(tt.in)
			if got != tt.want {
				t.Fatalf("SanitizeSessionName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- shellCommand (tmux-wrapped) tests ---

func TestShellCommandProducesTmux(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "agent-1", 0, 0, false)
	if len(got) != 3 || got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("shellCommand() = %v, want [sh -c ...]", got)
	}
	script := got[2]
	if !strings.Contains(script, "command -v tmux") {
		t.Errorf("script missing tmux install check: %s", script)
	}
	if !strings.Contains(script, "exec tmux -u new-session -A -s 'agent-1'") {
		t.Errorf("script missing tmux new-session -A: %s", script)
	}
	if !strings.Contains(script, "bind-key -n C-q detach-client") {
		t.Errorf("script missing ctrl+q keybinding: %s", script)
	}
	if !strings.Contains(script, "bind-key -n C-o detach-client") {
		t.Errorf("script missing ctrl+o keybinding: %s", script)
	}
}

func TestShellCommandClipboard(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "agent-1", 0, 0, false)
	script := got[2]
	if !strings.Contains(script, "set -g set-clipboard on") {
		t.Errorf("script missing set-clipboard on: %s", script)
	}
	if !strings.Contains(script, "terminal-features") || !strings.Contains(script, "clipboard") {
		t.Errorf("script missing terminal-features clipboard: %s", script)
	}
}

func TestShellCommandClipboardNested(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "agent-1", 80, 24, true)
	script := got[2]
	if !strings.Contains(script, "set -g set-clipboard on") {
		t.Errorf("nested script missing set-clipboard on: %s", script)
	}
	if !strings.Contains(script, "terminal-features") {
		t.Errorf("nested script missing terminal-features: %s", script)
	}
}

func TestShellCommandClipboardFeaturesIsLast(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "agent-1", 80, 24, false)
	script := got[2]
	featIdx := strings.LastIndex(script, "terminal-features")
	mouseIdx := strings.Index(script, "set -g mouse on")
	clipIdx := strings.Index(script, "set-clipboard on")
	if featIdx < 0 || mouseIdx < 0 || clipIdx < 0 {
		t.Fatalf("script missing expected settings: %s", script)
	}
	if featIdx < clipIdx {
		t.Errorf("terminal-features should come after set-clipboard (for tmux <3.2 compat): %s", script)
	}
	if featIdx < mouseIdx {
		t.Errorf("terminal-features should come after mouse on: %s", script)
	}
}

func TestShellCommandWithDotfilesAndTmux(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	config.DotfilesSettings.InstallScript = "install.sh"

	got := shellCommand(config, "worker-2", 0, 0, false)
	script := got[2]

	// Dotfiles setup should come before tmux
	dotIdx := strings.Index(script, "[ ! -d ~/dotfiles ]")
	tmuxIdx := strings.Index(script, "exec tmux")
	if dotIdx < 0 || tmuxIdx < 0 {
		t.Fatalf("script missing dotfiles or tmux: %s", script)
	}
	if dotIdx > tmuxIdx {
		t.Errorf("dotfiles setup should come before tmux: %s", script)
	}
	if !strings.Contains(script, "git clone 'https://github.com/user/dots' ~/dotfiles") {
		t.Errorf("script missing git clone: %s", script)
	}
	if !strings.Contains(script, "sh 'install.sh'") {
		t.Errorf("script missing install script: %s", script)
	}
	if strings.HasSuffix(script, "exec bash") {
		t.Errorf("script should not end with exec bash: %s", script)
	}
}

func TestShellCommandSanitizesSessionName(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "my.instance:1", 0, 0, false)
	script := got[2]
	if !strings.Contains(script, "tmux -u new-session -A -s 'my-instance-1'") {
		t.Errorf("script should sanitize session name: %s", script)
	}
}

// --- freshShellCommand tests ---

func TestFreshShellCommandNilConfig(t *testing.T) {
	got := freshShellCommand(nil)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(nil) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandEmptyDotfiles(t *testing.T) {
	config := state.DefaultConfig()
	got := freshShellCommand(config)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(empty) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandRepoOnlyNoScript(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	got := freshShellCommand(config)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(repo only) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandScriptOnlyNoRepo(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.InstallScript = "install.sh"
	got := freshShellCommand(config)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(script only) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandBothSet(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	config.DotfilesSettings.InstallScript = "install.sh"

	got := freshShellCommand(config)
	if len(got) != 3 {
		t.Fatalf("freshShellCommand() returned %d args, want 3", len(got))
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("freshShellCommand() = %v, want [sh -c ...]", got)
	}

	script := got[2]
	if !strings.Contains(script, "[ ! -d ~/dotfiles ]") {
		t.Error("script missing ~/dotfiles existence check")
	}
	if !strings.Contains(script, "git clone 'https://github.com/user/dots' ~/dotfiles") {
		t.Errorf("script missing git clone: %s", script)
	}
	if !strings.Contains(script, "sh 'install.sh'") {
		t.Errorf("script missing install script invocation: %s", script)
	}
	if !strings.HasSuffix(script, "exec bash") {
		t.Errorf("script should end with exec bash: %s", script)
	}
}

func TestShellCommandNestedNoInnerPaneKeys(t *testing.T) {
	config := state.DefaultConfig()
	got := shellCommand(config, "agent-1", 80, 24, true)
	script := got[2]
	// Pane navigation is handled by the outer tmux, so the inner
	// tmux should not bind j/k even when vim keys are enabled.
	if strings.Contains(script, "bind-key j select-pane") {
		t.Errorf("nested script should not have j keybinding (pane nav is on outer tmux): %s", script)
	}
	if strings.Contains(script, "bind-key k select-pane") {
		t.Errorf("nested script should not have k keybinding (pane nav is on outer tmux): %s", script)
	}
	// Should still have the prefix override for inner tmux,
	// session-scoped so it doesn't leak to full-screen attaches or a
	// user's own sessions on the shared server.
	if !strings.Contains(script, `set prefix C-x`) || strings.Contains(script, "set -g prefix") {
		t.Errorf("nested script should set session-scoped prefix: %s", script)
	}
	// The status bar doubles as the tab bar and auto-hides while the
	// session has a single window.
	if !strings.Contains(script, "set @fleet_tab_autohide 1") {
		t.Errorf("nested script missing tab auto-hide marker: %s", script)
	}
}

func TestShellCommandTabBar(t *testing.T) {
	config := state.DefaultConfig()
	toggle := `if -F '#{&&:#{@fleet_tab_autohide},#{==:#{session_windows},1}}' 'set status off' 'set status on'`
	for _, nested := range []bool{true, false} {
		script := shellCommand(config, "agent-1", 80, 24, nested)[2]
		for _, want := range []string{
			"set status-position top",
			"set -g window-status-format ' #I:#W '",
			`set-hook window-linked "` + toggle + `"`,
			`set-hook window-unlinked "` + toggle + `"`,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("nested=%v script missing %q: %s", nested, want, script)
			}
		}
	}
}

func TestShellCommandTabBarNested(t *testing.T) {
	config := state.DefaultConfig()
	script := shellCommand(config, "agent-1", 80, 24, true)[2]
	// Auto-hide on, and the initial toggle runs at attach so a reattach
	// to a multi-window session shows the bar immediately.
	if !strings.Contains(script, `set @fleet_tab_autohide 1 \; if -F`) {
		t.Errorf("nested script missing attach-time tab toggle: %s", script)
	}
	if !strings.Contains(script, "ctrl+x c: new tab") {
		t.Errorf("nested script missing new-tab hint: %s", script)
	}
}

func TestShellCommandTabBarFullScreen(t *testing.T) {
	config := state.DefaultConfig()
	script := shellCommand(config, "agent-1", 80, 24, false)[2]
	// Full-screen keeps the bar always on (it carries the detach hint).
	if !strings.Contains(script, `set @fleet_tab_autohide 0 \; set status on`) {
		t.Errorf("full-screen script should force status on: %s", script)
	}
	if !strings.Contains(script, "ctrl+q/ctrl+o: detach") {
		t.Errorf("full-screen script missing detach hint: %s", script)
	}
	// A prior nested attach session-scopes prefix C-x; full-screen must
	// reset it so the documented ctrl+b keys hold regardless of order.
	if !strings.Contains(script, `set -u prefix`) {
		t.Errorf("full-screen script should reset session prefix: %s", script)
	}
}

func TestShellCommandNestedVimKeysDisabled(t *testing.T) {
	config := state.DefaultConfig()
	off := false
	config.GeneralSettings.TmuxVimKeys = &off

	got := shellCommand(config, "agent-1", 80, 24, true)
	script := got[2]
	if strings.Contains(script, "bind-key j select-pane") {
		t.Errorf("nested script should not have j keybinding when vim keys disabled: %s", script)
	}
	if strings.Contains(script, "bind-key k select-pane") {
		t.Errorf("nested script should not have k keybinding when vim keys disabled: %s", script)
	}
	if strings.Contains(script, "j/k:") {
		t.Errorf("nested script should not have vim keys help text: %s", script)
	}
	// Should still have the prefix override
	if !strings.Contains(script, "set prefix C-x") {
		t.Errorf("nested script missing prefix override: %s", script)
	}
}

func TestShellCommandAutoInstallSkipsDotfiles(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	config.DotfilesSettings.InstallScript = "install.sh"
	config.DotfilesSettings.AutoInstall = true

	got := shellCommand(config, "agent-1", 80, 24, false)
	script := got[2]
	if strings.Contains(script, "dotfiles") {
		t.Errorf("script should not contain dotfiles setup when auto-install is on: %s", script)
	}
	if !strings.Contains(script, "exec tmux") {
		t.Errorf("script should still contain tmux: %s", script)
	}
}

func TestFreshShellCommandAutoInstallSkipsDotfiles(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	config.DotfilesSettings.InstallScript = "install.sh"
	config.DotfilesSettings.AutoInstall = true

	got := freshShellCommand(config)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(auto-install) = %v, want [bash]", got)
	}
}

func TestDotfilesSetupScriptIgnoresAutoInstall(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	config.DotfilesSettings.InstallScript = "install.sh"
	config.DotfilesSettings.AutoInstall = true

	got := dotfilesSetupScript(config)
	if got == "" {
		t.Fatal("dotfilesSetupScript should return script regardless of auto-install")
	}
	if !strings.Contains(got, "git clone") {
		t.Errorf("script missing git clone: %s", got)
	}
}

func TestFreshShellCommandQuotesSpecialCharacters(t *testing.T) {
	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "https://github.com/user/it's-dots"
	config.DotfilesSettings.InstallScript = "my script.sh"

	got := freshShellCommand(config)
	if len(got) != 3 {
		t.Fatalf("freshShellCommand() returned %d args, want 3", len(got))
	}

	script := got[2]
	if !strings.Contains(script, `'https://github.com/user/it'\''s-dots'`) {
		t.Errorf("script has incorrect repo quoting: %s", script)
	}
	if !strings.Contains(script, "'my script.sh'") {
		t.Errorf("script has incorrect script quoting: %s", script)
	}
}
