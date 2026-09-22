package tui

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
)

// theme_client.go is the TUI's persistence path for the color theme (issue
// #251). The theme is a CLIENT preference stored in the global Config's theme
// group — but like the armada registry (armada_client.go) it belongs to the
// machine the user sits at, so it is always read from and written to the
// LOCAL daemon, never the fleet the TUI is currently switched onto. A local
// TUI's m.config IS the local config, so it uses it directly; a remote-booted
// or armada-switched TUI goes through DialLocal.

// fetchThemeLocal reads the theme name from the LOCAL daemon's config.
// Package var so tests can stub the persistence seam.
var fetchThemeLocal = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), armadaLocalTimeout)
	defer cancel()
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	reply, err := conn.Service().GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		return "", err
	}
	return reply.GetConfig().GetTheme().GetName(), nil
}

// saveThemeLocal writes the theme name into the LOCAL daemon's config: a
// read-modify-write of the whole Config, since SetConfig replaces it. Package
// var so tests can stub the persistence seam.
var saveThemeLocal = func(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), armadaLocalTimeout)
	defer cancel()
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	reply, err := conn.Service().GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		return err
	}
	// Round-trip through the domain type so the write path is the same one
	// the settings page uses (a zero base: the server normalizes).
	c := protoconv.ConfigFromProto(reply.GetConfig(), &configutil.Config{})
	c.ThemeSettings.Name = name
	_, err = conn.Service().SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(c)})
	return err
}

// themeLoadedMsg carries the LOCAL daemon's saved theme name.
type themeLoadedMsg struct {
	name string
	err  error
}

// themeSavedMsg reports an asynchronous local save (remote TUIs only).
type themeSavedMsg struct {
	name string
	err  error
}

// fetchThemeCmd loads the theme preference from the LOCAL daemon.
func fetchThemeCmd() tea.Cmd {
	return func() tea.Msg {
		name, err := fetchThemeLocal()
		return themeLoadedMsg{name: name, err: err}
	}
}

// saveThemeCmd persists name on the LOCAL daemon off the Update loop
// (DialLocal may have to spawn the daemon first).
func saveThemeCmd(name string) tea.Cmd {
	return func() tea.Msg {
		return themeSavedMsg{name: name, err: saveThemeLocal(name)}
	}
}

// setTheme makes name the active look: rebuilds the styles, refreshes the
// copies the model holds (the spinner's style) and dresses the tmux panes.
// An unknown name is the Fleet look (theme.Lookup). It does not persist.
func (m *model) setTheme(name string) {
	t := theme.Lookup(name)
	m.themeName = t.Name
	applyTheme(t)
	m.spinner.Style = spinnerStyle
	m.agentSpinner.Style = agentWorkingStyle
	if m.inHostTmux {
		applyPaneChrome(t.Pane)
	}
}

// selectTheme applies name and persists it as the client preference. A local
// TUI writes it through its own config (the active daemon IS the local one);
// a remote TUI applies optimistically and saves to the local daemon
// asynchronously, reverting on failure. Returns the save command, if any.
//
// Remote saves are serialized: bubbletea runs every Cmd on its own goroutine,
// so two ←/→ presses would race their GetConfig→SetConfig round trips and the
// LAST TO LAND would win — an in-between theme on disk. At most one save is in
// flight; presses meanwhile only change the look, and the save's completion
// (handleThemeMsg) issues one more save if the look moved on.
func (m *model) selectTheme(name string) tea.Cmd {
	previous := m.themeName
	m.setTheme(name)
	m.themePicked = true
	if !fleetclient.IsRemote() {
		if m.config == nil {
			m.config = configutil.DefaultConfig()
		}
		m.config.ThemeSettings.Name = m.themeName
		if err := setConfigRemote(m.config); err != nil {
			m.config.ThemeSettings.Name = previous
			m.setTheme(previous)
			m.message = fmt.Sprintf("Failed to save settings: %v", err)
			return nil
		}
		m.themeSaved = m.themeName
		m.message = fmt.Sprintf("Theme set to %s", m.themeName)
		return nil
	}
	m.message = fmt.Sprintf("Theme set to %s", m.themeName)
	if m.themeSaving {
		return nil // coalesced: the in-flight save's completion saves this one
	}
	m.themeSaving = true
	return saveThemeCmd(m.themeName)
}

// handleThemeMsg applies the outcome of a load or an asynchronous save. It
// returns the follow-up save when the look moved on while one was in flight.
func (m *model) handleThemeMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case themeLoadedMsg:
		if msg.err != nil {
			// The local daemon should always be reachable (it auto-spawns);
			// keep the Fleet look rather than nag on the main page.
			return nil
		}
		if m.themePicked {
			// A slow boot-time load must not overwrite a theme the user has
			// since chosen (that choice is what is being persisted).
			return nil
		}
		m.setTheme(msg.name)
		m.themeSaved = m.themeName
	case themeSavedMsg:
		m.themeSaving = false
		saved := theme.Lookup(msg.name).Name
		if msg.err == nil {
			m.themeSaved = saved
		}
		if m.themeName != saved {
			// The user kept cycling while this save was in flight (presses
			// meanwhile start no save of their own): persist where they ended
			// up. That save reports — and reverts — on its own, so this one's
			// outcome only matters for themeSaved. Checked BEFORE the error
			// branch, or a failed save would strand the on-screen theme:
			// neither saved nor reverted.
			m.themeSaving = true
			return saveThemeCmd(m.themeName)
		}
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to save theme: %v", msg.err)
			m.setTheme(m.themeSaved) // back to the last persisted look
		}
	}
	return nil
}
