package tui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	tea "github.com/charmbracelet/bubbletea"
)

// The port-forward dialog's half-close toggle: 'h' (or tab in the add field)
// flips it, the dialog shows it, and the next forward asks the server for a
// connection that keeps reading after the local EOF.
func TestPortForwardDialogHalfCloseToggle(t *testing.T) {
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("a", fleet.StatusRunning, ""))
	m.portForwards = portforward.NewManager()
	defer m.portForwards.Shutdown()
	var asked []bool
	orig := forwardDialer
	forwardDialer = func(_, _ string, _ int, halfClose bool) (portforward.TargetDialer, error) {
		asked = append(asked, halfClose)
		return func() (io.ReadWriteCloser, error) { return nil, errors.New("not dialed in this test") }, nil
	}
	defer func() { forwardDialer = orig }()

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if fp.mode != viewPortForward {
		t.Fatalf("'p' left mode %v", fp.mode)
	}
	view := flatText(fp.renderPortForwardDialog(m))
	if !strings.Contains(view, "Half-close: off") || !strings.Contains(view, "[h] Half-close") {
		t.Fatalf("the toggle should show, off:\n%s", view)
	}
	fp.updatePortForward(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if !fp.pfHalfClose || fp.dlg.fieldActive {
		t.Fatalf("'h' should flip the toggle, not start typing: on=%v typing=%v", fp.pfHalfClose, fp.dlg.fieldActive)
	}
	if view := flatText(fp.renderPortForwardDialog(m)); !strings.Contains(view, "Half-close: on") {
		t.Fatalf("the toggle should show on:\n%s", view)
	}

	local := freeLocalPort(t)
	fp.updatePortForward(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(fmt.Sprintf("%d:80", local))})
	if !fp.dlg.fieldActive {
		t.Fatal("a digit should start the add field")
	}
	// tab in the field flips it too (and back).
	fp.updatePortForward(m, tea.KeyMsg{Type: tea.KeyTab})
	fp.updatePortForward(m, tea.KeyMsg{Type: tea.KeyTab})
	if !fp.pfHalfClose {
		t.Fatal("two tabs should leave the toggle on")
	}
	fp.updatePortForward(m, tea.KeyMsg{Type: tea.KeyEnter})
	key := "alpha/a"
	fwds := m.portForwards.List(key)
	if len(fwds) != 1 || !fwds[0].HalfClose || fwds[0].LocalPort != local || len(asked) != 1 || !asked[0] {
		t.Fatalf("the forward should be a half-close one: %+v (asked %v, message %q)", fwds, asked, m.message)
	}
	if !strings.Contains(m.message, "(half-close)") {
		t.Errorf("message %q", m.message)
	}
	if view := flatText(fp.renderPortForwardDialog(m)); !strings.Contains(view, fmt.Sprintf("%d->80 half-close", local)) {
		t.Errorf("the forward's row should say half-close:\n%s", view)
	}

	// Reopening the dialog starts with the toggle off.
	fp.closePortForward(m, key)
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if fp.pfHalfClose {
		t.Fatal("a reopened dialog should start with half-close off")
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
