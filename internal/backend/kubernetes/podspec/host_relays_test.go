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
// agent relay; any other daemon-host socket is recorded with its target, its
// source and its permission bits, and a bound socket directory with its
// target and source (fleetd lists its sockets while the pod runs).
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
	}
	if !slices.Equal(res.Pod.HostSockets, want) {
		t.Fatalf("host sockets %+v", res.Pod.HostSockets)
	}
	if got := res.Pod.HostSocketDirs; len(got) != 1 || got[0].Target != "/tmp/.X11-unix" || got[0].Source != "/home/dev/host/x11" || len(got[0].Shadowed) != 0 {
		t.Fatalf("host socket directories %+v", got)
	}
}

// TestHostSocketBindsShadowASocketDirectorysSocket: a bind at one of a bound
// socket directory's sockets (a socket, or the user's agent) is that socket,
// as the deeper mount is on docker: each target is the pod agent's once,
// since a state naming one twice is refused whole.
func TestHostSocketBindsShadowASocketDirectorysSocket(t *testing.T) {
	for name, bind := range map[string]string{
		"a socket":  "/home/dev/host/S.gpg-agent:/tmp/.X11-unix/X0",
		"the agent": "/home/dev/host/agent.sock:/tmp/.X11-unix/X0",
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput([]string{"-v", "/home/dev/host/x11:/tmp/.X11-unix", "-v", bind}, nil, nil)
			res, err := Translate(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range res.Pod.HostSockets {
				if s.Target == "/tmp/.X11-unix/X0" && s.Source != strings.Split(bind, ":")[0] {
					t.Fatalf("the directory's socket is relayed under the bind: %+v", res.Pod.HostSockets)
				}
			}
			dirs := res.Pod.HostSocketDirs
			if len(dirs) != 1 || !slices.Equal(dirs[0].Shadowed, []string{"X0"}) || dirs[0].Lists("X0") || !dirs[0].Lists("X1") {
				t.Fatalf("the directory does not leave the bound socket out: %+v", dirs)
			}
		})
	}
}

// TestHostSocketBindsWhereTheAgentBindsNone: a socket bound into a directory
// the pod's agent binds no host socket in (the kernel's, fleet's install) is
// refused at create, never left unbound.
func TestHostSocketBindsWhereTheAgentBindsNone(t *testing.T) {
	for _, target := range []string{"/sys/fs/s.sock", "/proc/s.sock"} {
		in := baseInput([]string{"-v", "/home/dev/host/S.gpg-agent:" + target}, nil, nil)
		if _, err := Translate(in); err == nil || !strings.Contains(err.Error(), CodeUnsupportedMount) || !strings.Contains(err.Error(), target) {
			t.Errorf("%s: %v", target, err)
		}
	}
	in := baseInput([]string{"-v", "/home/dev/host/x11:/sys/x11"}, nil, nil)
	if _, err := Translate(in); err == nil || !strings.Contains(err.Error(), "/sys/x11") {
		t.Errorf("a socket directory: %v", err)
	}
}

// TestHostSocketBindsUnderDev: a socket bound under /dev (a syslog socket at
// /dev/log, as docker binds one) is relayed: a pod's /dev is its container's
// own tmpfs.
func TestHostSocketBindsUnderDev(t *testing.T) {
	in := baseInput([]string{"-v", "/home/dev/host/S.gpg-agent:/dev/log", "-v", "/home/dev/host/x11:/dev/x11"}, nil, nil)
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range res.Pod.HostSockets {
		got[s.Target] = s.Source
	}
	if got["/dev/log"] != "/home/dev/host/S.gpg-agent" {
		t.Errorf("the /dev/log bind is not relayed: %+v", res.Pod.HostSockets)
	}
	if len(res.Pod.HostSocketDirs) != 1 || res.Pod.HostSocketDirs[0].Target != "/dev/x11" {
		t.Errorf("the socket directory under /dev is not relayed: %+v", res.Pod.HostSocketDirs)
	}
}

// TestHostSocketBindsPastTheLimits: a socket target a unix socket address
// cannot hold, and more socket binds or socket directories than a pod's
// agent relays, are refused, never dropped. A directory's own sockets are
// counted while the pod runs (fleetd's listing), not at create.
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
	if _, err := Translate(in); err != nil {
		t.Fatalf("a directory of many sockets: %v", err)
	}
	var args []string
	for i := 0; i <= podwire.MaxHostSocketDirs; i++ {
		args = append(args, "-v", fmt.Sprintf("/home/dev/host/many:/run/many%02d", i))
	}
	in = baseInput(args, nil, nil)
	in.Host = host
	if _, err := Translate(in); err == nil || !strings.Contains(err.Error(), CodeUnsupportedMount) || !strings.Contains(err.Error(), "socket directories") {
		t.Fatalf("too many socket directories: %v", err)
	}
	in = baseInput([]string{"-v", "/home/dev/host/x11:/" + strings.Repeat("d", podwire.MaxSocketPath)}, nil, nil)
	if _, err := Translate(in); err == nil || !strings.Contains(err.Error(), "holds at most") {
		t.Fatalf("a socket directory no socket path in fits: %v", err)
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
