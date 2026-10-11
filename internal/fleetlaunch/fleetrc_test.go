package fleetlaunch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
)

func TestRenderFleetRCExportsInstanceName(t *testing.T) {
	got := renderFleetRC("builder-1", peerDomain{})
	if !strings.HasPrefix(got, fleetRCContent) {
		t.Fatalf("rendered rc does not start with the embedded base rc")
	}
	if !strings.Contains(got, "export FLEET_INSTANCE_NAME='builder-1'\n") {
		t.Fatalf("rendered rc missing instance name export, got:\n%s", got)
	}
}

func TestRenderFleetRCQuotesSingleQuotesInName(t *testing.T) {
	got := renderFleetRC("o'brien", peerDomain{})
	want := `export FLEET_INSTANCE_NAME='o'\''brien'`
	if !strings.Contains(got, want) {
		t.Fatalf("rendered rc missing escaped export %q, got:\n%s", want, got)
	}
}

func TestRenderFleetRCEmptyNameLeavesBaseUnchanged(t *testing.T) {
	if got := renderFleetRC("", peerDomain{}); got != fleetRCContent {
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

// TestRenderFleetRCExportsThePeerDomain: with a peer domain, sourcing the rc
// exports FLEET_PEER_DOMAIN under the cluster domain the pod's DNS search
// list names (its namespace's svc domain), else under <ns>.svc, which the
// search list resolves too; without one the rc exports none.
func TestRenderFleetRCExportsThePeerDomain(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	rc := renderFleetRC("alpha", peerDomain{service: "fs-fleet-man-abcd2345-net", namespace: "fleet"})
	for _, tt := range []struct{ resolv, want string }{
		{"search fleet.svc.cluster.local svc.cluster.local cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n", "fs-fleet-man-abcd2345-net.fleet.svc.cluster.local"},
		{"search other.svc.corp.example fleet.svc.corp.example corp.example\n", "fs-fleet-man-abcd2345-net.fleet.svc.corp.example"},
		{"nameserver 1.1.1.1\n", "fs-fleet-man-abcd2345-net.fleet.svc"},
	} {
		dir := t.TempDir()
		resolv := filepath.Join(dir, "resolv.conf")
		if err := os.WriteFile(resolv, []byte(tt.resolv), 0o644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "fleet.rc")
		// The rc as staged, reading this test's resolv.conf.
		if err := os.WriteFile(file, []byte(strings.ReplaceAll(rc, "/etc/resolv.conf", resolv)), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sh, "-c", `. "$1" >/dev/null 2>&1; printf '%s|%s' "$FLEET_PEER_DOMAIN" "$(env | grep -c '^FLEET_PEER_DOMAIN=')"`, "sh", file).Output()
		if err != nil {
			t.Fatalf("sourcing the rc: %v", err)
		}
		if got := string(out); got != tt.want+"|1" {
			t.Errorf("resolv.conf %q: FLEET_PEER_DOMAIN|exported = %q, want %q", tt.resolv, got, tt.want+"|1")
		}
	}
	if strings.Contains(renderFleetRC("alpha", peerDomain{}), "FLEET_PEER_DOMAIN") {
		t.Error("an rc without a peer domain names one")
	}
}

type peerBackend struct {
	backend.Backend
	svc, ns string
}

func (p peerBackend) PeerDomain() (string, string, bool) { return p.svc, p.ns, p.svc != "" }

func TestPeerDomainOf(t *testing.T) {
	if got := peerDomainOf(peerBackend{svc: "s", ns: "n"}); got != (peerDomain{service: "s", namespace: "n"}) {
		t.Fatalf("%+v", got)
	}
	if got := peerDomainOf(peerBackend{}); got != (peerDomain{}) {
		t.Fatalf("%+v", got)
	}
}
