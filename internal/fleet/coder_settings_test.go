package fleet

import (
	"strings"
	"testing"
)

func TestNormalizeCoderSettingsTrimsAndKeepsValid(t *testing.T) {
	s := FleetSettings{
		CoderTemplate:      "  tmpl  ",
		CoderPreset:        " large ",
		CoderWorkspaceName: " my-Proj ",
		CoderParameters: []CoderParameter{
			{Name: " repo ", Value: "v"},
			{Name: "   ", Value: "dropped"},
		},
	}
	if err := NormalizeCoderSettings(&s); err != nil {
		t.Fatalf("NormalizeCoderSettings: %v", err)
	}
	if s.CoderTemplate != "tmpl" || s.CoderPreset != "large" || s.CoderWorkspaceName != "my-proj" {
		t.Fatalf("trimming failed: %+v", s)
	}
	if len(s.CoderParameters) != 1 || s.CoderParameters[0].Name != "repo" {
		t.Fatalf("empty-name param not dropped / name not trimmed: %+v", s.CoderParameters)
	}
}

func TestNormalizeCoderSettingsEmptyParamsBecomeNil(t *testing.T) {
	s := FleetSettings{CoderParameters: []CoderParameter{{Name: "  "}}}
	if err := NormalizeCoderSettings(&s); err != nil {
		t.Fatalf("NormalizeCoderSettings: %v", err)
	}
	if s.CoderParameters != nil {
		t.Fatalf("want nil params, got %+v", s.CoderParameters)
	}
}

func TestNormalizeCoderSettingsRejectsBadWorkspaceName(t *testing.T) {
	bad := []string{
		"has space",
		"-leading",
		"trailing-",
		"under_score",
		"dot.dot",
		strings.Repeat("a", 25), // over the 24-char prefix budget
	}
	for _, name := range bad {
		s := FleetSettings{CoderWorkspaceName: name}
		if err := NormalizeCoderSettings(&s); err == nil {
			t.Errorf("workspace name %q: want error, got nil", name)
		}
	}
	// Empty (use the fleet name) and legal fragments pass.
	for _, name := range []string{"", "a", "my-proj-2", "ABC-123"} {
		s := FleetSettings{CoderWorkspaceName: name}
		if err := NormalizeCoderSettings(&s); err != nil {
			t.Errorf("workspace name %q: unexpected error %v", name, err)
		}
	}
}

// TestNormalizeCoderSettingsLowercasesWorkspaceName pins the stored override
// to what `coder create` actually receives (coder names are lowercase).
func TestNormalizeCoderSettingsLowercasesWorkspaceName(t *testing.T) {
	s := FleetSettings{CoderWorkspaceName: " My-Proj "}
	if err := NormalizeCoderSettings(&s); err != nil {
		t.Fatalf("NormalizeCoderSettings: %v", err)
	}
	if s.CoderWorkspaceName != "my-proj" {
		t.Fatalf("CoderWorkspaceName = %q, want %q", s.CoderWorkspaceName, "my-proj")
	}
}

// TestNormalizeCoderSettingsDoesNotMutateCallerParams guards the exported
// contract: filtering must not scribble over the caller's backing array.
func TestNormalizeCoderSettingsDoesNotMutateCallerParams(t *testing.T) {
	orig := []CoderParameter{{Name: "  "}, {Name: "keep"}}
	s := FleetSettings{CoderParameters: orig}
	if err := NormalizeCoderSettings(&s); err != nil {
		t.Fatalf("NormalizeCoderSettings: %v", err)
	}
	if orig[0].Name != "  " || orig[1].Name != "keep" {
		t.Fatalf("caller slice mutated: %+v", orig)
	}
	if len(s.CoderParameters) != 1 || s.CoderParameters[0].Name != "keep" {
		t.Fatalf("filtered params wrong: %+v", s.CoderParameters)
	}
}
