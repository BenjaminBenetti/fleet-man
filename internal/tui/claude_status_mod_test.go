package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The fleet status mod is a global setting, on by default, in its own
// Claude Code section.
func TestClaudeStatusModDefaultsOn(t *testing.T) {
	sp, m := newMicTestModel(t)
	if !slices.Contains(visibleSettingsItems(sp, m), settingsItemClaudeStatusMod) {
		t.Fatal("Fleet status mod row missing")
	}
	view := sp.View(m)
	if !strings.Contains(view, "Claude Code") || !strings.Contains(view, "Fleet status mod") {
		t.Fatalf("Claude Code section not rendered:\n%s", view)
	}
	if !m.config.ClaudeCodeSettings.StatusModEnabled() {
		t.Fatal("the fleet status mod must default to on")
	}
}

func TestToggleClaudeStatusModPersists(t *testing.T) {
	sp, m := newMicTestModel(t)

	for _, want := range []bool{false, true} {
		sp.cursor = settingsPositionOf(sp, m, settingsItemClaudeStatusMod)
		sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
		loaded, err := state.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := loaded.ClaudeCodeSettings.StatusModEnabled(); got != want || m.config.ClaudeCodeSettings.StatusModEnabled() != want {
			t.Fatalf("after toggling: saved %v, shown %v, want %v", got, m.config.ClaudeCodeSettings.StatusModEnabled(), want)
		}
	}
}

func TestToggleClaudeStatusModRevertsOnSaveFailure(t *testing.T) {
	sp, m := newMicTestModel(t)
	setConfigRemote = func(*state.Config) error { return errors.New("daemon unreachable") }
	sp.cursor = settingsPositionOf(sp, m, settingsItemClaudeStatusMod)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !m.config.ClaudeCodeSettings.StatusModEnabled() {
		t.Fatal("a failed save must leave the toggle where it was")
	}
	if !strings.Contains(m.message, "Failed to save settings") {
		t.Fatalf("message = %q", m.message)
	}
}
