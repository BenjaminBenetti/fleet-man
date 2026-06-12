package fleet

import (
	"strings"
	"testing"
)

func TestNormalizeLayoutPresetTrimsName(t *testing.T) {
	p, err := NormalizeLayoutPreset(LayoutPreset{Name: "  dev  ", PaneCommands: []string{"npm run dev"}})
	if err != nil {
		t.Fatalf("NormalizeLayoutPreset: %v", err)
	}
	if p.Name != "dev" {
		t.Fatalf("name not trimmed: %q", p.Name)
	}
}

func TestNormalizeLayoutPresetRejectsEmptyName(t *testing.T) {
	if _, err := NormalizeLayoutPreset(LayoutPreset{Name: "   ", PaneCommands: []string{""}}); err == nil {
		t.Fatal("want error for empty name")
	}
}

func TestNormalizeLayoutPresetRejectsNoPanes(t *testing.T) {
	if _, err := NormalizeLayoutPreset(LayoutPreset{Name: "dev"}); err == nil {
		t.Fatal("want error for zero panes")
	}
}

func TestNormalizeLayoutPresetRejectsTooManyPanes(t *testing.T) {
	if _, err := NormalizeLayoutPreset(LayoutPreset{Name: "dev", PaneCommands: make([]string, maxLayoutPresetPanes+1)}); err == nil {
		t.Fatal("want error for too many panes")
	}
}

func TestNormalizeLayoutPresetAllowsEmptyCommands(t *testing.T) {
	// "" per pane means a plain shell — explicitly legal.
	p, err := NormalizeLayoutPreset(LayoutPreset{Name: "dev", PaneCommands: []string{"", ""}})
	if err != nil {
		t.Fatalf("NormalizeLayoutPreset: %v", err)
	}
	if p.PaneCount() != 2 {
		t.Fatalf("pane count = %d, want 2", p.PaneCount())
	}
}

func TestNormalizeLayoutPresetsEmptyInputIsNil(t *testing.T) {
	out, err := NormalizeLayoutPresets(nil)
	if err != nil || out != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", out, err)
	}
}

func TestNormalizeLayoutPresetsRejectsDuplicateNames(t *testing.T) {
	_, err := NormalizeLayoutPresets([]LayoutPreset{
		{Name: "dev", PaneCommands: []string{""}},
		{Name: " dev ", PaneCommands: []string{""}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-name error, got %v", err)
	}
}

func TestNormalizeLayoutPresetsPreservesOrder(t *testing.T) {
	out, err := NormalizeLayoutPresets([]LayoutPreset{
		{Name: "b", PaneCommands: []string{"x"}},
		{Name: "a", PaneCommands: []string{"y", "z"}},
	})
	if err != nil {
		t.Fatalf("NormalizeLayoutPresets: %v", err)
	}
	if len(out) != 2 || out[0].Name != "b" || out[1].Name != "a" {
		t.Fatalf("order not preserved: %v", out)
	}
}
