package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// resetTheme puts the package back on the Fleet look after a test that
// switched it (the styles are package-level).
func resetTheme(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		applyTheme(theme.Fleet())
		paneChromeSaved = nil
	})
}

// TestApplyThemeRebuildsStyles confirms a theme switch reaches the package
// styles, the instance color cycle and the model's spinner copy, and that
// switching back to Fleet restores the original look exactly.
func TestApplyThemeRebuildsStyles(t *testing.T) {
	resetTheme(t)
	fleetTitle := titleStyle.GetForeground()
	fleetPurple := instanceColorStyle("purple").GetForeground()

	m := &model{spinner: spinner.New()}
	m.setTheme("Gruvbox Dark")
	if m.themeName != "Gruvbox Dark" {
		t.Fatalf("themeName = %q", m.themeName)
	}
	if activeTheme.Name != "Gruvbox Dark" || titleStyle.GetForeground() != theme.GruvboxDark().Accent {
		t.Fatalf("styles not rebuilt: activeTheme=%q title=%v", activeTheme.Name, titleStyle.GetForeground())
	}
	if got := instanceColorStyle("purple").GetForeground(); got != theme.GruvboxDark().Instance.Purple {
		t.Fatalf("instance palette not rebuilt: purple=%v", got)
	}
	if m.spinner.Style.GetForeground() != theme.GruvboxDark().Accent {
		t.Fatal("spinner style not refreshed")
	}

	m.setTheme("no such theme")
	if m.themeName != theme.Default || titleStyle.GetForeground() != fleetTitle || instanceColorStyle("purple").GetForeground() != fleetPurple {
		t.Fatal("unknown theme must restore the Fleet look exactly")
	}
}

// TestSelectThemeLocalPersistsThroughConfig: a local TUI's active daemon IS
// the local one, so the theme rides the ordinary config save and the model's
// config copy carries it (a later unrelated save must not revert it).
func TestSelectThemeLocalPersistsThroughConfig(t *testing.T) {
	resetTheme(t)
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvServer, "")

	var saved *configutil.Config
	orig := setConfigRemote
	setConfigRemote = func(c *configutil.Config) error {
		cp := *c
		saved = &cp
		return nil
	}
	t.Cleanup(func() { setConfigRemote = orig })
	localCalled := false
	origLocal := saveThemeLocal
	saveThemeLocal = func(string) error { localCalled = true; return nil }
	t.Cleanup(func() { saveThemeLocal = origLocal })

	sp := newSettingsPage()
	m := &model{config: state.DefaultConfig(), toolStatus: allToolsFound(), spinner: spinner.New()}
	if cmd := sp.cycleTheme(m, 1); cmd != nil {
		t.Fatal("local save is synchronous: no command expected")
	}
	if saved == nil || saved.ThemeSettings.Name != "Gruvbox Dark" || m.config.ThemeSettings.Name != "Gruvbox Dark" {
		t.Fatalf("theme not persisted through config: saved=%+v model=%+v", saved, m.config.ThemeSettings)
	}
	if localCalled {
		t.Fatal("a local TUI must not take the DialLocal path")
	}
	if m.themeSaved != "Gruvbox Dark" || !strings.Contains(m.message, "Gruvbox Dark") {
		t.Fatalf("themeSaved=%q message=%q", m.themeSaved, m.message)
	}

	// Backwards from Fleet wraps to the last theme; a failed save reverts.
	setConfigRemote = func(*configutil.Config) error { return errors.New("boom") }
	sp.cycleTheme(m, -1)
	if m.themeName != "Gruvbox Dark" || m.config.ThemeSettings.Name != "Gruvbox Dark" || activeTheme.Name != "Gruvbox Dark" {
		t.Fatalf("failed save must revert: name=%q config=%q active=%q", m.themeName, m.config.ThemeSettings.Name, activeTheme.Name)
	}
	if !strings.Contains(m.message, "Failed to save") {
		t.Fatalf("message = %q", m.message)
	}
}

// TestSelectThemeRemoteSavesLocally: a remote-booted TUI must not write the
// theme into the REMOTE fleet's config; it applies optimistically and saves to
// the local daemon asynchronously, reverting to the last persisted look when
// that fails.
func TestSelectThemeRemoteSavesLocally(t *testing.T) {
	resetTheme(t)
	t.Setenv(fleetclient.EnvGateway, "https://gw.example")

	remoteSaved := false
	orig := setConfigRemote
	setConfigRemote = func(*configutil.Config) error { remoteSaved = true; return nil }
	t.Cleanup(func() { setConfigRemote = orig })
	var localName string
	origLocal := saveThemeLocal
	saveThemeLocal = func(name string) error { localName = name; return nil }
	t.Cleanup(func() { saveThemeLocal = origLocal })

	m := &model{config: state.DefaultConfig(), spinner: spinner.New(), themeName: theme.Default, themeSaved: theme.Default}
	cmd := m.selectTheme("Catppuccin Latte")
	if cmd == nil {
		t.Fatal("remote save must be asynchronous")
	}
	if remoteSaved || m.config.ThemeSettings.Name != "" {
		t.Fatal("remote TUI must not write the theme into the remote config")
	}
	if activeTheme.Name != "Catppuccin Latte" {
		t.Fatal("theme must apply optimistically")
	}
	msg := cmd()
	if localName != "Catppuccin Latte" {
		t.Fatalf("local save got %q", localName)
	}
	m.handleThemeMsg(msg)
	if m.themeSaved != "Catppuccin Latte" {
		t.Fatalf("themeSaved = %q", m.themeSaved)
	}

	// A failed async save reverts to the last persisted theme.
	saveThemeLocal = func(string) error { return errors.New("daemon down") }
	cmd = m.selectTheme("Tokyo Night")
	m.handleThemeMsg(cmd())
	if activeTheme.Name != "Catppuccin Latte" || m.themeName != "Catppuccin Latte" {
		t.Fatalf("failed async save must revert: active=%q", activeTheme.Name)
	}
	if !strings.Contains(m.message, "Failed to save theme") {
		t.Fatalf("message = %q", m.message)
	}

	// The boot-time load applies whatever the local daemon has.
	m.handleThemeMsg(themeLoadedMsg{name: "Solarized Light"})
	if activeTheme.Name != "Solarized Light" || m.themeSaved != "Solarized Light" {
		t.Fatal("themeLoadedMsg must apply and record the persisted name")
	}
	m.handleThemeMsg(themeLoadedMsg{err: errors.New("nope")})
	if activeTheme.Name != "Solarized Light" {
		t.Fatal("a failed load must keep the current look")
	}
}

// TestThemeRowInSettings: the row sits in General and renders the current
// theme's name.
func TestThemeRowInSettings(t *testing.T) {
	resetTheme(t)
	sp := newSettingsPage()
	m := &model{config: state.DefaultConfig(), toolStatus: allToolsFound(), spinner: spinner.New(), width: 120, themeName: "Tokyo Night"}
	items := sp.visibleItems(m)
	at := slices.Index(items, settingsItemTheme)
	if at == -1 || items[at-1] != settingsItemShowHelpText {
		t.Fatalf("theme row must follow Show help text in General: %v", items)
	}
	out := sp.viewSettings(m)
	if !strings.Contains(out, "Theme") || !strings.Contains(out, "[ Tokyo Night ]") || !strings.Contains(out, "dark") {
		t.Fatalf("settings view missing the theme row:\n%s", out)
	}
	// Enter cycles forward like ←/→ (through the same persistence path).
	orig := setConfigRemote
	setConfigRemote = func(*configutil.Config) error { return nil }
	t.Cleanup(func() { setConfigRemote = orig })
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvServer, "")
	sp.cursor = settingsPositionOf(sp, m, settingsItemTheme)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.themeName != "Gruvbox Light" {
		t.Fatalf("enter should step to the next theme, got %q", m.themeName)
	}
}

// TestPaneChromeSaveApplyRestore pins the tmux command sequence: the window's
// OWN values are read once (no -A), set for a themed look, and put back — as
// a re-set where there was one, an unset where there was not — on restore or
// when the Fleet theme (no chrome) is applied.
func TestPaneChromeSaveApplyRestore(t *testing.T) {
	resetTheme(t)
	var calls []string
	orig := tmuxRun
	tmuxRun = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "show-options" && args[2] == "pane-active-border-style" {
			return "fg=yellow", nil // the user had a window-level active style
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxRun = orig })

	restorePaneChrome() // before any apply: nothing to do
	if len(calls) != 0 {
		t.Fatalf("restore before apply ran %v", calls)
	}

	applyPaneChrome(theme.GruvboxDark().Pane)
	want := []string{
		"show-options -wv pane-border-style",
		"show-options -wv pane-active-border-style",
		"set-option -w pane-border-style fg=#504945",
		"set-option -w pane-active-border-style fg=#fabd2f",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("apply calls:\n%v\nwant\n%v", calls, want)
	}

	calls = nil
	applyPaneChrome(theme.CatppuccinMocha().Pane) // second apply: no re-save
	want = []string{
		"set-option -w pane-border-style fg=#45475a",
		"set-option -w pane-active-border-style fg=#cba6f7",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("second apply calls:\n%v\nwant\n%v", calls, want)
	}

	calls = nil
	applyPaneChrome(theme.Fleet().Pane) // Fleet = restore
	want = []string{
		"set-option -wu pane-border-style",
		"set-option -w pane-active-border-style fg=yellow",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("Fleet apply (restore) calls:\n%v\nwant\n%v", calls, want)
	}
}
