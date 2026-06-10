package fleetlaunch

import (
	"strings"
	"testing"
)

func TestRenderFleetRCExportsInstanceName(t *testing.T) {
	got := renderFleetRC("builder-1")
	if !strings.HasPrefix(got, fleetRCContent) {
		t.Fatalf("rendered rc does not start with the embedded base rc")
	}
	if !strings.Contains(got, "export FLEET_INSTANCE_NAME='builder-1'\n") {
		t.Fatalf("rendered rc missing instance name export, got:\n%s", got)
	}
}

func TestRenderFleetRCQuotesSingleQuotesInName(t *testing.T) {
	got := renderFleetRC("o'brien")
	want := `export FLEET_INSTANCE_NAME='o'\''brien'`
	if !strings.Contains(got, want) {
		t.Fatalf("rendered rc missing escaped export %q, got:\n%s", want, got)
	}
}

func TestRenderFleetRCEmptyNameLeavesBaseUnchanged(t *testing.T) {
	if got := renderFleetRC(""); got != fleetRCContent {
		t.Fatalf("empty instance name should return the embedded rc unchanged, got:\n%s", got)
	}
}
