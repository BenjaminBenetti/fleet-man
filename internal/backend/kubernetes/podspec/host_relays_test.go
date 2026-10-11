package podspec

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestHostSocketBinds: a bind of the user's agent socket is a link to fleet's
// agent relay; any other daemon-host socket, and each socket (only) of a
// bound socket directory, is recorded with its target, its source and its
// permission bits.
func TestHostSocketBinds(t *testing.T) {
	in := baseInput([]string{
		"-v", "/home/dev/host/agent.sock:/run/ssh.sock",
		"--mount", "type=bind,source=/home/dev/host/S.gpg-agent,target=/home/vscode/.gnupg/S.gpg-agent",
		"-v", "/home/dev/host/x11:/tmp/.X11-unix",
	}, nil, nil)
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Pod.PID1.Links, []podinit.Link{{Path: "/run/ssh.sock", Target: "/fleet-mounts/control/ssh-agent.sock"}}; !slices.Equal(got, want) {
		t.Fatalf("links %+v", got)
	}
	want := []hostrelay.Socket{
		{Target: "/home/vscode/.gnupg/S.gpg-agent", Source: "/home/dev/host/S.gpg-agent", Mode: 0o600},
		{Target: "/tmp/.X11-unix/X0", Source: "/home/dev/host/x11/X0", Mode: 0o777},
	}
	if !slices.Equal(res.Pod.HostSockets, want) {
		t.Fatalf("host sockets %+v", res.Pod.HostSockets)
	}
}

// TestHostSocketBindsPastTheLimits: a socket target a unix socket address
// cannot hold, and more sockets than a pod's agent relays, are refused, never
// dropped.
func TestHostSocketBindsPastTheLimits(t *testing.T) {
	long := "/" + strings.Repeat("d", podwire.MaxSocketPath) + "/gpg.sock"
	in := baseInput([]string{"-v", "/home/dev/host/S.gpg-agent:" + long}, nil, nil)
	if _, err := Translate(in); !strings.Contains(err.Error(), CodeUnsupportedMount) || !strings.Contains(err.Error(), "holds at most") {
		t.Fatalf("a long target: %v", err)
	}

	host := maps.Clone(baseHost)
	host["/home/dev/host/many"] = fs.ModeDir | 0o755
	for i := 0; i <= podwire.MaxHostSockets; i++ {
		host[fmt.Sprintf("/home/dev/host/many/s%02d", i)] = fs.ModeSocket | 0o600
	}
	in = baseInput([]string{"-v", "/home/dev/host/many:/run/many"}, nil, nil)
	in.Host = host
	if _, err := Translate(in); !strings.Contains(err.Error(), CodeUnsupportedMount) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many sockets: %v", err)
	}
}

// TestHostGateway: host-gateway mirrors the daemon host's ports but for those
// the devcontainer serves itself (published container ports, forwardPorts
// of this container), and warns of the mirror's deltas; on the node's
// network nothing is mirrored, with a warning.
func TestHostGateway(t *testing.T) {
	in := baseInput([]string{"--add-host", "host.docker.internal:host-gateway", "-p", "127.0.0.1:8000:3000"}, nil, nil)
	in.Merged = json.RawMessage(`{"remoteUser":"vscode","forwardPorts":[3000, 5173, "localhost:9229", "db:5432", "x"]}`)
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pod.HostGateway || !slices.Equal(res.Pod.GatewaySkip, []uint16{3000, 5173, 9229}) {
		t.Fatalf("gateway %v skip %v", res.Pod.HostGateway, res.Pod.GatewaySkip)
	}
	if !slices.ContainsFunc(res.Pod.Warnings, func(w string) bool { return strings.Contains(w, "loopback-only ones too") }) {
		t.Fatalf("no warning of the mirror's deltas: %q", res.Pod.Warnings)
	}

	in = baseInput([]string{"--add-host", "host.docker.internal:host-gateway", "--network", "host"}, nil, nil)
	local(&in)
	res, err = Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pod.HostGateway || len(res.Pod.GatewaySkip) != 0 {
		t.Fatal("a host-network pod mirrors the daemon host's ports")
	}
	if !slices.ContainsFunc(res.Pod.Warnings, func(w string) bool { return strings.Contains(w, "node's network") }) {
		t.Fatalf("no warning for the host-network pod: %q", res.Pod.Warnings)
	}
}

// TestHostGatewayResolvesToTheAgentsAddress: the pod's host.docker.internal
// is the address the pod agent mirrors the daemon host's ports on.
func TestHostGatewayResolvesToTheAgentsAddress(t *testing.T) {
	res, err := Translate(baseInput([]string{"--add-host", "host.docker.internal:host-gateway"}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Pod.HostAliases; len(a) != 1 || a[0].IP != podwire.HostGatewayAddr {
		t.Fatalf("host aliases %+v", a)
	}
}
