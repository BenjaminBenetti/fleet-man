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
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 0, 0, false, true, true)
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
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 0, 0, false, true, true)
	script := got[2]
	if !strings.Contains(script, "set -g set-clipboard on") {
		t.Errorf("script missing set-clipboard on: %s", script)
	}
	if !strings.Contains(script, "terminal-features") || !strings.Contains(script, "clipboard") {
		t.Errorf("script missing terminal-features clipboard: %s", script)
	}
}

func TestShellCommandClipboardNested(t *testing.T) {
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 80, 24, true, true, true)
	script := got[2]
	if !strings.Contains(script, "set -g set-clipboard on") {
		t.Errorf("nested script missing set-clipboard on: %s", script)
	}
	if !strings.Contains(script, "terminal-features") {
		t.Errorf("nested script missing terminal-features: %s", script)
	}
}

func TestShellCommandClipboardFeaturesIsLast(t *testing.T) {
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 80, 24, false, true, true)
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
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"

	got := shellCommand(cfg, "worker-2", 0, 0, false, true, true)
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
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "my.instance:1", 0, 0, false, true, true)
	script := got[2]
	if !strings.Contains(script, "tmux -u new-session -A -s 'my-instance-1'") {
		t.Errorf("script should sanitize session name: %s", script)
	}
}

// --- freshShellCommand tests ---

func TestFreshShellCommandNilConfig(t *testing.T) {
	got := freshShellCommand(nil, true)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(nil, true) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandEmptyDotfiles(t *testing.T) {
	cfg := state.DefaultConfig()
	got := freshShellCommand(cfg, true)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(empty) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandRepoOnlyNoScript(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	got := freshShellCommand(cfg, true)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(repo only) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandScriptOnlyNoRepo(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.InstallScript = "install.sh"
	got := freshShellCommand(cfg, true)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(script only) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandBothSet(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"

	got := freshShellCommand(cfg, true)
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
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 80, 24, true, true, true)
	script := got[2]
	// Pane navigation is handled by the outer tmux, so the inner
	// tmux should not bind j/k even when vim keys are enabled.
	if strings.Contains(script, "bind-key j select-pane") {
		t.Errorf("nested script should not have j keybinding (pane nav is on outer tmux): %s", script)
	}
	if strings.Contains(script, "bind-key k select-pane") {
		t.Errorf("nested script should not have k keybinding (pane nav is on outer tmux): %s", script)
	}
	// Should still have the prefix override for inner tmux.
	if !strings.Contains(script, "set -g prefix C-x") {
		t.Errorf("nested script missing prefix override: %s", script)
	}
	// Status bar should be hidden in nested mode.
	if !strings.Contains(script, "set -g status off") {
		t.Errorf("nested script should hide status bar: %s", script)
	}
}

func TestShellCommandNestedVimKeysDisabled(t *testing.T) {
	cfg := state.DefaultConfig()
	off := false
	cfg.GeneralSettings.TmuxVimKeys = &off

	got := shellCommand(cfg, "agent-1", 80, 24, true, true, true)
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
	if !strings.Contains(script, "set -g prefix C-x") {
		t.Errorf("nested script missing prefix override: %s", script)
	}
}

func TestShellCommandAutoInstallSkipsDotfiles(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"
	cfg.DotfilesSettings.AutoInstall = true

	got := shellCommand(cfg, "agent-1", 80, 24, false, true, true)
	script := got[2]
	if strings.Contains(script, "dotfiles") {
		t.Errorf("script should not contain dotfiles setup when auto-install is on: %s", script)
	}
	if !strings.Contains(script, "exec tmux") {
		t.Errorf("script should still contain tmux: %s", script)
	}
}

func TestFreshShellCommandAutoInstallSkipsDotfiles(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"
	cfg.DotfilesSettings.AutoInstall = true

	got := freshShellCommand(cfg, true)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(auto-install) = %v, want [bash]", got)
	}
}

func TestDotfilesSetupScriptIgnoresAutoInstall(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"
	cfg.DotfilesSettings.AutoInstall = true

	got := dotfilesSetupScript(cfg)
	if got == "" {
		t.Fatal("dotfilesSetupScript should return script regardless of auto-install")
	}
	if !strings.Contains(got, "git clone") {
		t.Errorf("script missing git clone: %s", got)
	}
}

func TestShellCommandSkipsDotfilesWhenBackendUnsupported(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"
	// AutoInstall=false so dotfiles WOULD normally be injected at attach time.

	got := shellCommand(cfg, "agent-1", 0, 0, false, false, true)
	script := got[2]
	if strings.Contains(script, "dotfiles") {
		t.Errorf("script should not reference dotfiles when supportsDotfiles=false: %s", script)
	}
	if !strings.Contains(script, "exec tmux") {
		t.Errorf("script should still contain tmux: %s", script)
	}
}

func TestShellCommandIncludesAgentForwardingFixWhenSupported(t *testing.T) {
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 0, 0, false, true, true)
	script := got[2]
	if !strings.Contains(script, "/run/ssh-agent.sock") {
		t.Errorf("script should contain SSH agent socket relink when supportsAgentForwarding=true: %s", script)
	}
}

func TestShellCommandSkipsAgentForwardingFixWhenUnsupported(t *testing.T) {
	cfg := state.DefaultConfig()
	got := shellCommand(cfg, "agent-1", 0, 0, false, true, false)
	script := got[2]
	if strings.Contains(script, "/run/ssh-agent.sock") {
		t.Errorf("script should NOT touch /run/ssh-agent.sock when supportsAgentForwarding=false: %s", script)
	}
	if strings.Contains(script, "ssh_auth_sock") {
		t.Errorf("script should NOT relink SSH_AUTH_SOCK when supportsAgentForwarding=false: %s", script)
	}
	if !strings.Contains(script, "exec tmux") {
		t.Errorf("script should still launch tmux: %s", script)
	}
}

func TestFreshShellCommandSkipsDotfilesWhenBackendUnsupported(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/dots"
	cfg.DotfilesSettings.InstallScript = "install.sh"

	got := freshShellCommand(cfg, false)
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("freshShellCommand(supportsDotfiles=false) = %v, want [bash]", got)
	}
}

func TestFreshShellCommandQuotesSpecialCharacters(t *testing.T) {
	cfg := state.DefaultConfig()
	cfg.DotfilesSettings.RepoURL = "https://github.com/user/it's-dots"
	cfg.DotfilesSettings.InstallScript = "my script.sh"

	got := freshShellCommand(cfg, true)
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
