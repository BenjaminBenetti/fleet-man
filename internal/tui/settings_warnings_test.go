package tui

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
)

type settingsWarningServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
	warnings []string
}

func (*settingsWarningServer) Hello(_ context.Context, req *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	return &fleetgrpc.HelloReply{ServerVersion: req.GetClientVersion()}, nil
}

func (s *settingsWarningServer) SetFleetSettings(context.Context, *fleetgrpc.SetFleetSettingsRequest) (*fleetgrpc.MutationReply, error) {
	return &fleetgrpc.MutationReply{Warnings: s.warnings}, nil
}

// The daemon's warnings about saved settings come back with the save, over
// the same RPC a remote TUI uses.
func TestSetFleetSettingsRemoteReturnsTheWarnings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvToken, "")
	mutConnMu.Lock()
	previous := mutConn
	mutConn = nil
	mutConnMu.Unlock()
	t.Cleanup(func() {
		closeMutationConn()
		mutConnMu.Lock()
		mutConn = previous
		mutConnMu.Unlock()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	want := []string{"custom mount /srv: host folder /data/gone does not exist on the daemon host"}
	fleetgrpc.RegisterFleetServiceServer(server, &settingsWarningServer{warnings: want})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	t.Setenv(fleetclient.EnvServer, listener.Addr().String())

	got, err := setFleetSettingsRemote("alpha", fleet.FleetSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("warnings %q, want %q", got, want)
	}
}

// A save the daemon warns about shows its warning on the message line,
// after the save's own message: a missing host folder from the custom mount
// editor, a login the agent dialog's cluster target lacks.
func TestSettingsSavesShowTheDaemonsWarnings(t *testing.T) {
	stubWarnings := func(t *testing.T, warnings ...string) {
		orig := setFleetSettingsRemote
		setFleetSettingsRemote = func(string, fleet.FleetSettings) ([]string, error) { return warnings, nil }
		t.Cleanup(func() { setFleetSettingsRemote = orig })
	}

	t.Run("host folder", func(t *testing.T) {
		const missing = "custom mount /opt/data: host folder /data/big does not exist on the daemon host"
		stubWarnings(t, missing)
		f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{CustomMounts: []string{"/opt/data"}}}
		fp := newFleetPage()
		fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
		m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp, width: 120, height: 50}
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
		navigateToCustomMountRow(t, fp, m, 0)
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		fp.mountHostInput.SetValue("/data/big")
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
		if !strings.Contains(m.message, missing) {
			t.Fatalf("message %q, want the daemon's warning", m.message)
		}
		if view := fp.viewFleetList(m); !strings.Contains(view, "does not exist on the daemon host") {
			t.Fatalf("the warning is not on screen with the dialog open:\n%s", view)
		}
	})

	t.Run("agent", func(t *testing.T) {
		const login = "agent builder: log in to Claude once for this fleet on ☸ prod; until then its firings are refused"
		m, fp := newAutomationModel(t)
		stubWarnings(t, login)
		fp.openAddAgentDialog(m, "alpha")
		fp.agentDlg.name = "builder"
		fp.saveAutomationAgent(m)
		if !strings.HasPrefix(m.message, `Saved agent "builder"`) || !strings.Contains(m.message, login) {
			t.Fatalf("message %q, want the save and the daemon's warning", m.message)
		}
	})

	t.Run("none", func(t *testing.T) {
		m, fp := newAutomationModel(t)
		fp.openAddAgentDialog(m, "alpha")
		fp.agentDlg.name = "builder"
		fp.saveAutomationAgent(m)
		if m.message != `Saved agent "builder"` {
			t.Fatalf("message %q", m.message)
		}
	})
}
