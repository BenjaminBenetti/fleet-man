package fleet

import (
	"fmt"
	"strings"
)

// Layout presets (issue #150) are saved pane-layout templates a user can apply
// when creating a new session. A preset is captured FROM a live session group:
// Layout is the outer-tmux window_layout string the group save/restore
// mechanism already persists (state.GroupLayout.Layout — leaf 0 is the fleet
// TUI pane), and PaneCommands holds one bash command per non-TUI pane in
// position order (top then left, the same ordering GroupLayout.Sessions uses).
// As with custom mounts, the TUI validates for immediate UX feedback and the
// server validates authoritatively in SetFleetSettings, both through the one
// definition below.

// maxLayoutPresetPanes bounds a preset's pane count. The outer tmux window
// cannot usefully hold more panes than this, and the bound keeps a corrupt or
// hostile settings write from minting an absurd number of sessions on apply.
const maxLayoutPresetPanes = 32

// LayoutPreset is one saved session-layout template.
type LayoutPreset struct {
	// Name is the user-chosen label shown when Tab-cycling templates in the
	// new-session dialog. Unique within a fleet's preset list.
	Name string `json:"name"`

	// Layout is the captured outer-tmux window_layout string ("" when the
	// source group had no captured geometry; applying such a preset falls back
	// to the default stacked split, exactly like a GroupLayout with no layout).
	Layout string `json:"layout,omitempty"`

	// PaneCommands holds the startup command for each non-TUI pane in position
	// order (top then left). "" means the pane opens a plain shell. The slice
	// length IS the preset's pane count.
	PaneCommands []string `json:"paneCommands"`
}

// PaneCount returns the number of session panes the preset creates.
func (p LayoutPreset) PaneCount() int { return len(p.PaneCommands) }

// NormalizeLayoutPreset validates a single preset and returns its canonical
// form: the name is whitespace-trimmed and must be non-empty, and the preset
// must have between 1 and maxLayoutPresetPanes pane commands (a preset with no
// panes would create nothing). Commands themselves are arbitrary bash run
// inside the user's own instance, so they are deliberately not inspected.
func NormalizeLayoutPreset(p LayoutPreset) (LayoutPreset, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return LayoutPreset{}, fmt.Errorf("layout preset name is empty")
	}
	if len(p.PaneCommands) == 0 {
		return LayoutPreset{}, fmt.Errorf("layout preset %q has no panes", p.Name)
	}
	if len(p.PaneCommands) > maxLayoutPresetPanes {
		return LayoutPreset{}, fmt.Errorf("layout preset %q has %d panes (max %d)", p.Name, len(p.PaneCommands), maxLayoutPresetPanes)
	}
	return p, nil
}

// NormalizeLayoutPresets validates every entry and returns the normalized list
// (input order preserved). An empty or nil input yields a nil slice and no
// error. A duplicate name — after trimming — is an error rather than a silent
// dedup: unlike custom mounts, two presets with one name are not redundant
// no-ops, they are ambiguous, so the whole update is rejected.
func NormalizeLayoutPresets(in []LayoutPreset) ([]LayoutPreset, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]LayoutPreset, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		norm, err := NormalizeLayoutPreset(raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[norm.Name]; dup {
			return nil, fmt.Errorf("duplicate layout preset name %q", norm.Name)
		}
		seen[norm.Name] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}
