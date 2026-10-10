package tui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// kube_helpers_test.go: shared fixtures for the Kubernetes Settings tests.

// kubeTestModel is a model on the Settings page with the given connections
// pushed, and a local (not remote) daemon.
func kubeTestModel(t *testing.T, clusters ...*fleetgrpc.KubeClusterStatus) (*model, *settingsPage) {
	t.Helper()
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvServer, "")
	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		spinner:     spinner.New(),
		width:       130,
		st:          &state.State{Fleets: map[string]*fleet.Fleet{}},
		daemonTools: &daemonToolStatus{host: "ben-desktop"},
		currentPage: sp,
	}
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: clusters}
	return m, sp
}

// kubeSt is a connection status for tests.
func kubeSt(id, name string, state fleetgrpc.KubeClusterState) *fleetgrpc.KubeClusterStatus {
	return &fleetgrpc.KubeClusterStatus{
		Cluster: &fleetgrpc.KubeCluster{Id: id, DisplayName: name, Context: name + "-ctx", Namespace: "fleet",
			KubeconfigPaths: []string{"/home/ben/.kube/config"}, Revision: 3},
		State: state,
	}
}

// flatText is a rendered view as plain words: styles and box borders
// stripped and all whitespace (wrapping included) collapsed to one space.
func flatText(s string) string {
	s = ansi.Strip(s)
	s = strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// kubeKey builds the KeyMsg whose String() is s.
func kubeKeyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	if len(s) > 5 && s[:5] == "ctrl+" && len(s) == 6 {
		return tea.KeyMsg{Type: tea.KeyCtrlA + tea.KeyType(s[5]-'a')}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press sends key s to the Settings page and returns its command.
func press(m *model, sp *settingsPage, s string) tea.Cmd {
	return sp.Update(m, kubeKeyMsg(s))
}

// pumpKube runs cmd and every command its messages lead to, dispatching
// each message the way model.Update does for the Kubernetes messages (the
// rest go to the current page). A command that does not return at once is a
// timer (a cursor blink) and is dropped. It returns the messages delivered.
func pumpKube(t *testing.T, m *model, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var delivered []tea.Msg
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 500 {
			t.Fatal("pumpKube: the commands never settle")
		}
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg, ok := runQuickly(next)
		if !ok || msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		delivered = append(delivered, msg)
		switch msg := msg.(type) {
		case kubeTestMsg:
			queue = append(queue, m.handleKubeTestMsg(msg))
		case kubeLoginMsg:
			queue = append(queue, m.handleKubeLoginMsg(msg))
		case kubePluginsMsg:
			m.handleKubePluginsMsg(msg)
		case kubeClustersMsg:
			queue = append(queue, m.handleKubeClustersMsg(msg))
		default:
			queue = append(queue, m.currentPage.Update(m, msg))
		}
	}
	return delivered
}

// runQuickly runs cmd, giving up on one that blocks (a tea.Tick).
func runQuickly(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(200 * time.Millisecond):
		return nil, false
	}
}

// fakeTestStream replays TestKubeCluster events, then io.EOF (or err).
type fakeTestStream struct {
	mu     sync.Mutex
	events []*fleetgrpc.TestKubeClusterEvent
	err    error
}

func (s *fakeTestStream) Recv() (*fleetgrpc.TestKubeClusterEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

// testEvents is a streamed preflight: each check, then the result.
func testEvents(result *fleetgrpc.KubeClusterStatus, checks ...*fleetgrpc.KubeCheck) []*fleetgrpc.TestKubeClusterEvent {
	var out []*fleetgrpc.TestKubeClusterEvent
	for _, c := range checks {
		out = append(out, &fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Check{Check: c}})
	}
	return append(out, &fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Result{Result: result}})
}

// stubKubeTest makes TestKubeCluster replay events, recording each request.
func stubKubeTest(t *testing.T, events func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent) *[]*fleetgrpc.TestKubeClusterRequest {
	t.Helper()
	var reqs []*fleetgrpc.TestKubeClusterRequest
	var mu sync.Mutex
	orig := openKubeTestRemote
	openKubeTestRemote = func(_ context.Context, req *fleetgrpc.TestKubeClusterRequest) (kubeTestStream, error) {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		return &fakeTestStream{events: events(req)}, nil
	}
	t.Cleanup(func() { openKubeTestRemote = orig })
	return &reqs
}

// stubKubeContexts makes ListKubeContexts answer reply, recording the paths
// asked for.
func stubKubeContexts(t *testing.T, reply *fleetgrpc.ListKubeContextsReply) *[][]string {
	t.Helper()
	var asked [][]string
	orig := listKubeContextsRemote
	listKubeContextsRemote = func(paths []string) (*fleetgrpc.ListKubeContextsReply, error) {
		asked = append(asked, paths)
		return reply, nil
	}
	t.Cleanup(func() { listKubeContextsRemote = orig })
	return &asked
}

// kubeUpdateCall is one UpdateKubeCluster the editor sent.
type kubeUpdateCall struct {
	id      string
	cluster *fleetgrpc.KubeCluster
	paths   []string
	rev     int64
}

// stubKubeUpdate records UpdateKubeCluster calls and answers with reply(call).
func stubKubeUpdate(t *testing.T, reply func(kubeUpdateCall) (*fleetgrpc.KubeClusterStatus, error)) *[]kubeUpdateCall {
	t.Helper()
	var calls []kubeUpdateCall
	var mu sync.Mutex
	orig := updateKubeClusterRemote
	updateKubeClusterRemote = func(id string, c *fleetgrpc.KubeCluster, paths []string, rev int64) (*fleetgrpc.KubeClusterStatus, error) {
		call := kubeUpdateCall{id, c, paths, rev}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		return reply(call)
	}
	t.Cleanup(func() { updateKubeClusterRemote = orig })
	return &calls
}
