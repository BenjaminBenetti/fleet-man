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
	if s.CoderTemplate != "tmpl" || s.CoderPreset != "large" || s.CoderWorkspaceName != "my-Proj" {
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
