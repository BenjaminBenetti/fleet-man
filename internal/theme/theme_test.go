package theme

import (
	"reflect"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestAllThemesComplete guards every slot of every theme: a forgotten slot
// renders as "no color", which on the right terminal is invisible text.
func TestAllThemesComplete(t *testing.T) {
	seen := map[string]bool{}
	dark, light := 0, 0
	for _, th := range All() {
		if th.Name == "" {
			t.Fatal("theme with an empty name")
		}
		if seen[th.Name] {
			t.Fatalf("duplicate theme name %q", th.Name)
		}
		seen[th.Name] = true
		if th.Dark {
			dark++
		} else {
			light++
		}
		v := reflect.ValueOf(th)
		colorType := reflect.TypeOf(lipgloss.Color(""))
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			switch f.Type {
			case colorType:
				if v.Field(i).String() == "" {
					t.Errorf("%s: slot %s is empty", th.Name, f.Name)
				}
			case reflect.TypeOf(InstancePalette{}):
				p := v.Field(i)
				for j := 0; j < p.NumField(); j++ {
					if p.Field(j).String() == "" {
						t.Errorf("%s: instance color %s is empty", th.Name, p.Type().Field(j).Name)
					}
				}
			case reflect.TypeOf(RGB{}):
				if v.Field(i).IsZero() {
					t.Errorf("%s: gradient endpoint %s is zero (black)", th.Name, f.Name)
				}
			}
		}
		// Every theme but Fleet dresses the tmux panes; Fleet must leave them
		// alone (that IS the pre-theme look).
		if th.Name == Default {
			if th.Pane != (PaneChrome{}) {
				t.Errorf("Fleet must not set tmux pane chrome: %+v", th.Pane)
			}
		} else if th.Pane.Border == "" || th.Pane.ActiveBorder == "" {
			t.Errorf("%s: pane chrome incomplete: %+v", th.Name, th.Pane)
		}
	}
	if dark != 4 || light != 4 {
		t.Fatalf("expected 4 dark + 4 light themes, got %d dark, %d light", dark, light)
	}
}

// TestFleetIsOriginalLook pins the Fleet theme to the ANSI-256 indices the
// TUI shipped with before themes: it is the default, and "the current look"
// must stay byte-identical for users who never touch the setting.
func TestFleetIsOriginalLook(t *testing.T) {
	f := Fleet()
	want := map[string]lipgloss.Color{
		"Accent": "170", "Primary": "39", "Border": "63", "Success": "42", "Warning": "214",
		"Error": "196", "ErrorBg": "52", "Danger": "203", "Purple": "99", "Info": "44",
		"Muted": "240", "Subtle": "241", "Faint": "238", "Collapsed": "245", "Text": "252", "Message": "229",
	}
	v := reflect.ValueOf(f)
	for slot, code := range want {
		if got := lipgloss.Color(v.FieldByName(slot).String()); got != code {
			t.Errorf("Fleet.%s = %q, want %q", slot, got, code)
		}
	}
	if f.Instance != (InstancePalette{Red: "196", Orange: "214", Yellow: "226", Green: "42", Cyan: "39", Blue: "69", Purple: "170", Pink: "213"}) {
		t.Errorf("Fleet instance palette drifted: %+v", f.Instance)
	}
	if !f.Dark || f.Name != Default {
		t.Errorf("Fleet must be the dark default: %+v", f)
	}
}

func TestLookupFallsBackToFleet(t *testing.T) {
	if Lookup("").Name != Default || Lookup("no such theme").Name != Default {
		t.Fatal("unknown / empty names must resolve to Fleet")
	}
	if Lookup("Catppuccin Latte").Name != "Catppuccin Latte" {
		t.Fatal("known name must resolve to itself")
	}
}

func TestNextWraps(t *testing.T) {
	names := Names()
	if names[0] != Default {
		t.Fatalf("Fleet must lead the cycle, got %q", names[0])
	}
	if got := Next(names[len(names)-1], 1); got != names[0] {
		t.Errorf("forward wrap: got %q want %q", got, names[0])
	}
	if got := Next(names[0], -1); got != names[len(names)-1] {
		t.Errorf("backward wrap: got %q want %q", got, names[len(names)-1])
	}
	if got := Next("bogus", 1); got != names[1] {
		t.Errorf("unknown current counts as Fleet: got %q want %q", got, names[1])
	}
	if Fleet().Kind() != "dark" || SolarizedLight().Kind() != "light" {
		t.Error("Kind labels wrong")
	}
}
