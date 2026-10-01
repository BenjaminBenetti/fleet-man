package fleetlaunch

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
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

// TestFleetRCHandsAgentsTheFleetMCP: the rc keys the fleet MCP off the socket
// the daemon serves — the path the bridge dials — and only then runs `fleet
// mcp-env`, so with the fleet setting off (no socket) a shell is untouched.
func TestFleetRCHandsAgentsTheFleetMCP(t *testing.T) {
	want := "if [ -S " + mcpbridge.ContainerSocketPath + " ] && command -v fleet >/dev/null 2>&1; then\n  eval \"$(fleet mcp-env 2>/dev/null)\"\nfi\n"
	if !strings.Contains(fleetRCContent, want) {
		t.Fatalf("fleet.rc should hand agents the fleet MCP when %s exists; want it to contain:\n%s", mcpbridge.ContainerSocketPath, want)
	}
}
