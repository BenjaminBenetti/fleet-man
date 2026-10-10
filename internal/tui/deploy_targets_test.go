package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

var (
	devcontainerTarget = fleet.DeployTarget{Backend: fleet.BackendDevcontainer}
	coderTarget        = fleet.DeployTarget{Backend: fleet.BackendCoder}
	codespacesTarget   = fleet.DeployTarget{Backend: fleet.BackendCodespaces}
)

func stubCheckTools(t *testing.T, tools []deps.ToolStatus) {
	t.Helper()
	orig := checkTools
	checkTools = func() []deps.ToolStatus { return tools }
	t.Cleanup(func() { checkTools = orig })
}

// stubListDeployTargets answers ListDeployTargets with reply/err and returns
// the fleet names it was asked about.
func stubListDeployTargets(t *testing.T, reply *fleetgrpc.ListDeployTargetsReply, err error) *[]string {
	t.Helper()
	var asked []string
	orig := listDeployTargetsRemote
	listDeployTargetsRemote = func(fleetName string) (*fleetgrpc.ListDeployTargetsReply, error) {
		asked = append(asked, fleetName)
		return reply, err
	}
	t.Cleanup(func() { listDeployTargetsRemote = orig })
	return &asked
}

func deployTargetsReply(def fleet.DeployTarget, options ...fleet.DeployTargetOption) *fleetgrpc.ListDeployTargetsReply {
	return &fleetgrpc.ListDeployTargetsReply{
		Targets:       protoconv.DeployTargetOptionsToProto(options),
		DefaultTarget: protoconv.DeployTargetToProto(def),
	}
}

func available(target fleet.DeployTarget) fleet.DeployTargetOption {
	return fleet.DeployTargetOption{Target: target, Available: true}
}

func unavailable(target fleet.DeployTarget) fleet.DeployTargetOption {
	return fleet.DeployTargetOption{Target: target, Reason: target.Tool() + " CLI not found where fleetd runs"}
}

// pressAddOnFleet presses 'a' on the header of a git fleet named alpha.
func pressAddOnFleet(t *testing.T, config *configutil.Config) (*fleetPage, *model) {
	t.Helper()
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Remote: "git@x:alpha.git"}}},
		fleetPage: fp,
		config:    config,
		creating:  map[string]bool{},
	}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	return fp, m
}

// The Deploy row is the daemon's: its available targets, opening on its
// remembered default, never this machine's PATH.
func TestAddInstanceKeyUsesTheDaemonsTargets(t *testing.T) {
	stubCheckTools(t, nil) // this machine has nothing installed; it must not matter
	asked := stubListDeployTargets(t, deployTargetsReply(codespacesTarget,
		available(devcontainerTarget), unavailable(coderTarget), available(codespacesTarget)), nil)

	fp, m := pressAddOnFleet(t, &configutil.Config{DefaultBackend: string(fleet.BackendCoder)})

	if fp.mode != viewAddInstance {
		t.Fatalf("mode = %v, want viewAddInstance (message %q)", fp.mode, m.message)
	}
	if len(*asked) != 1 || (*asked)[0] != "alpha" {
		t.Fatalf("ListDeployTargets asked for %v, want [alpha]", *asked)
	}
	if fp.addInst.target != codespacesTarget {
		t.Fatalf("dialog opened on %v, want the daemon's remembered codespaces (not this TUI's config copy)", fp.addInst.target)
	}
	want := []fleet.DeployTarget{devcontainerTarget, codespacesTarget}
	if len(fp.addInst.targets) != len(want) || fp.addInst.targets[0] != want[0] || fp.addInst.targets[1] != want[1] {
		t.Fatalf("Deploy row cycles %v, want %v (unavailable coder left out)", fp.addInst.targets, want)
	}

	// Cycling skips the unavailable target and wraps. The Name field is active
	// as the dialog opens; esc leaves it so tab reaches the Deploy row.
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEsc})
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyTab})
	if fp.addInst.target != devcontainerTarget {
		t.Fatalf("tab moved to %v, want devcontainer", fp.addInst.target)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyTab})
	if fp.addInst.target != codespacesTarget {
		t.Fatalf("second tab moved to %v, want codespaces", fp.addInst.target)
	}
	if view := fp.renderAddInstanceDialog(m); !strings.Contains(view, "[ Codespaces ]") || !strings.Contains(view, "tab] Cycle") {
		t.Fatalf("dialog should show the target and offer cycling:\n%s", view)
	}
}

func TestAddInstanceKeyOpensOnFirstAvailableWhenDefaultIsNot(t *testing.T) {
	stubListDeployTargets(t, deployTargetsReply(coderTarget,
		available(devcontainerTarget), unavailable(coderTarget), available(codespacesTarget)), nil)

	fp, m := pressAddOnFleet(t, nil)
	if fp.mode != viewAddInstance || fp.addInst.target != devcontainerTarget {
		t.Fatalf("mode %v target %v, want the dialog open on devcontainer (message %q)", fp.mode, fp.addInst.target, m.message)
	}
}

// A newer daemon may offer a kind of target this build can't name: it reads
// back as the zero target and must not be offered as a second Devcontainer,
// nor picked as the default.
func TestAddInstanceKeySkipsTargetsThisBuildCannotName(t *testing.T) {
	future := &fleetgrpc.DeployTarget{Backend: fleetgrpc.BackendType(99)}
	stubListDeployTargets(t, &fleetgrpc.ListDeployTargetsReply{
		Targets: []*fleetgrpc.DeployTargetOption{
			{Target: protoconv.DeployTargetToProto(devcontainerTarget), Available: true},
			{Target: future, Available: true},
			{Target: protoconv.DeployTargetToProto(codespacesTarget), Available: true},
		},
		DefaultTarget: future,
	}, nil)

	fp, m := pressAddOnFleet(t, nil)
	if fp.mode != viewAddInstance || fp.addInst.target != devcontainerTarget {
		t.Fatalf("mode %v target %v, want the dialog open on devcontainer (message %q)", fp.mode, fp.addInst.target, m.message)
	}
	want := []fleet.DeployTarget{devcontainerTarget, codespacesTarget}
	if len(fp.addInst.targets) != len(want) || fp.addInst.targets[0] != want[0] || fp.addInst.targets[1] != want[1] {
		t.Fatalf("Deploy row cycles %v, want %v (the unknown target left out)", fp.addInst.targets, want)
	}
}

// The Codespaces "set the Machine field" dialog sends the user to Settings.
// The Codespaces section there is gated on this machine's gh, which opening
// the New instance dialog used to look up on the way: Settings must check it
// itself, or the Machine field the dialog points at is hidden.
func TestSettingsFromTheCodespacesMachineDialogShowsTheMachineField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubCheckTools(t, allToolsFound())
	fp := newFleetPage()
	fp.mode = viewCodespacesMachine
	m := &model{config: state.DefaultConfig(), fleetPage: fp, currentPage: fp, creating: map[string]bool{}}

	fp.updateCodespacesMachine(m, tea.KeyMsg{Type: tea.KeyEnter})
	sp, ok := m.currentPage.(*settingsPage)
	if !ok {
		t.Fatalf("current page = %T, want the settings page", m.currentPage)
	}
	if settingsPositionOf(sp, m, settingsItemCodespacesMachine) < 0 {
		t.Fatal("the Codespaces Machine field is hidden although gh is installed here")
	}
	if got := toolStatusItems(sp, m); len(got) != len(allToolsFound()) {
		t.Fatalf("tool status rows = %v, want one per tool on this machine", got)
	}
}

func TestAddInstanceKeyWithNoAvailableTarget(t *testing.T) {
	stubListDeployTargets(t, deployTargetsReply(devcontainerTarget,
		unavailable(devcontainerTarget), unavailable(coderTarget), unavailable(codespacesTarget)), nil)

	fp, m := pressAddOnFleet(t, nil)
	if fp.mode == viewAddInstance {
		t.Fatal("dialog opened with no available target")
	}
	if !strings.Contains(m.message, "No deploy targets available") {
		t.Fatalf("message = %q", m.message)
	}
}

func TestAddInstanceKeyReportsAnUnreachableDaemon(t *testing.T) {
	stubCheckTools(t, allToolsFound())
	stubListDeployTargets(t, nil, grpcstatus.Error(grpccodes.Unavailable, "connection refused"))

	fp, m := pressAddOnFleet(t, nil)
	if fp.mode == viewAddInstance {
		t.Fatal("dialog opened although the daemon could not be asked")
	}
	if m.message != "Could not list deploy targets: connection refused" {
		t.Fatalf("message = %q", m.message)
	}
}

// A daemon from before ListDeployTargets: the TUI judges by this machine's
// PATH and its config copy, as it always did.
func TestAddInstanceKeyAgainstADaemonWithoutDeployTargets(t *testing.T) {
	stubCheckTools(t, []deps.ToolStatus{
		{Binary: "devcontainer", Found: true},
		{Binary: "gh", Found: true},
	})
	stubListDeployTargets(t, nil, grpcstatus.Error(grpccodes.Unimplemented, "unknown method ListDeployTargets"))

	fp, m := pressAddOnFleet(t, &configutil.Config{DefaultBackend: string(fleet.BackendCodespaces)})
	if fp.mode != viewAddInstance {
		t.Fatalf("mode = %v (message %q)", fp.mode, m.message)
	}
	if fp.addInst.target != codespacesTarget {
		t.Fatalf("target = %v, want the config's codespaces", fp.addInst.target)
	}
	if len(fp.addInst.targets) != 2 {
		t.Fatalf("targets = %v, want devcontainer and codespaces (no coder CLI here)", fp.addInst.targets)
	}
}

// The create carries remember_default and no whole-Config save happens: the
// daemon records the default, so a stale config copy can't clobber anything.
func TestSubmitAddInstanceRemembersTheTargetServerSide(t *testing.T) {
	origSet := setConfigRemote
	setConfigRemote = func(*configutil.Config) error {
		t.Error("the New instance dialog must not save the whole config")
		return nil
	}
	t.Cleanup(func() { setConfigRemote = origSet })
	var got fleet.DeployTarget
	origCreate := createInstanceRemote
	createInstanceRemote = func(_, _, _, _ string, target fleet.DeployTarget) error {
		got = target
		return nil
	}
	t.Cleanup(func() { createInstanceRemote = origCreate })

	stubListDeployTargets(t, deployTargetsReply(devcontainerTarget, available(devcontainerTarget), available(codespacesTarget)), nil)
	fp, m := pressAddOnFleet(t, &configutil.Config{DefaultBackend: string(fleet.BackendDevcontainer)})
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEsc})
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyTab})
	fp.textInput.SetValue("i1")
	cmd := fp.submitAddInstance(m)
	if cmd == nil {
		t.Fatalf("submit returned no command (message %q)", m.message)
	}
	cmd()
	if got != codespacesTarget {
		t.Fatalf("created on %v, want codespaces", got)
	}
	if m.config.DefaultBackend != string(fleet.BackendCodespaces) {
		t.Fatalf("config copy default = %q, want it to mirror codespaces for the agent dialog", m.config.DefaultBackend)
	}
	if !strings.Contains(m.message, "(Codespaces)") {
		t.Fatalf("message = %q", m.message)
	}

	req := newCreateInstanceRequest("alpha", "i1", "git@x:alpha.git", "", codespacesTarget)
	if !req.GetRememberDefault() || req.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES {
		t.Fatalf("create request = %v, want codespaces with remember_default", req)
	}
}

// Editing shows where the instance runs and can't change it.
func TestEditInstanceShowsItsDeployTarget(t *testing.T) {
	fp := newFleetPage()
	inst := &fleet.Instance{Name: "one", Backend: fleet.BackendCodespaces}
	f := &fleet.Fleet{Name: "alpha", Remote: "git@x:alpha.git", Instances: []*fleet.Instance{inst}}
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.openEditInstanceDialog(m)
	if fp.addInst.target != codespacesTarget || fp.addInst.targets != nil {
		t.Fatalf("edit dialog target %v targets %v", fp.addInst.target, fp.addInst.targets)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyTab})
	if fp.addInst.target != codespacesTarget {
		t.Fatalf("tab changed the target of an existing instance to %v", fp.addInst.target)
	}
	if view := fp.renderAddInstanceDialog(m); !strings.Contains(view, "Codespaces") {
		t.Fatalf("edit dialog should show the instance's target:\n%s", view)
	}
}

// ===========================================
// Settings -> Tool Status
// ===========================================

func toolStatusItems(sp *settingsPage, m *model) []int {
	var out []int
	for _, id := range sp.visibleItems(m) {
		if id >= settingsItemToolStatusBase && id < settingsItemDoctor {
			out = append(out, id)
		}
	}
	return out
}

// The row count follows the tool lists: this machine's (4 on WSL, where the
// fixed count of 5 left a phantom row) plus the daemon host's once known.
func TestToolStatusRowsFollowBothToolLists(t *testing.T) {
	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound()[:4],
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
		width:       160,
	}
	if got := toolStatusItems(sp, m); len(got) != 4 {
		t.Fatalf("tool status rows = %v, want 4 for this machine's 4 tools", got)
	}
	if view := sp.viewSettings(m); !strings.Contains(view, "checking the tools where fleetd runs") {
		t.Fatalf("view should say the fleetd tools are being checked:\n%s", view)
	}

	// The answer lands while the cursor sits below the section: it stays on
	// the same row though rows were inserted above it.
	sp.cursor = settingsPositionOf(sp, m, settingsItemDoctor)
	m.handleDaemonToolsMsg(daemonToolsMsg{gen: m.watchGen, status: daemonToolStatus{
		host: "build-box",
		tools: []deps.ToolStatus{
			{Name: "docker", Binary: "docker", Found: true, Description: "Builds and runs devcontainer instances."},
			{Name: "devcontainer", Binary: "devcontainer", Found: false, Description: "Creates devcontainer instances.", InstallURL: "https://example.com/devcontainer"},
		},
	}})
	if got := sp.settingsCursorItem(m); got != settingsItemDoctor {
		t.Fatalf("cursor moved to item %d, want it kept on Doctor", got)
	}
	got := toolStatusItems(sp, m)
	if len(got) != 6 || got[4] != settingsItemDaemonToolBase || got[5] != settingsItemDaemonToolBase+1 {
		t.Fatalf("tool status rows = %v, want 4 local then 2 fleetd rows", got)
	}
	view := sp.viewSettings(m)
	for _, want := range []string{"this machine", "on fleetd @ build-box", "not found"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}

	// Enter on a fleetd row opens its install page.
	origOpen := openURL
	var opened string
	openURL = func(url string) { opened = url }
	t.Cleanup(func() { openURL = origOpen })
	sp.cursor = settingsPositionOf(sp, m, settingsItemDaemonToolBase+1)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if opened != "https://example.com/devcontainer" {
		t.Fatalf("enter on the fleetd devcontainer row opened %q", opened)
	}
}

func TestDaemonToolsFromAnotherConnectionAreDropped(t *testing.T) {
	m := &model{watchGen: 2}
	m.handleDaemonToolsMsg(daemonToolsMsg{gen: 1, status: daemonToolStatus{host: "old"}})
	if m.daemonTools != nil {
		t.Fatal("an answer from before an armada switch was kept")
	}
}

func TestDaemonToolsErrorIsShownWithoutRows(t *testing.T) {
	sp := newSettingsPage()
	m := &model{config: state.DefaultConfig(), toolStatus: allToolsFound(), currentPage: sp, fleetPage: newFleetPage(), spinner: spinner.New(), width: 160}
	m.handleDaemonToolsMsg(daemonToolsMsg{status: daemonToolStatus{err: "this fleetd is too old to report its tools"}})
	if got := toolStatusItems(sp, m); len(got) != len(m.toolStatus) {
		t.Fatalf("tool status rows = %v, want only this machine's", got)
	}
	if view := sp.viewSettings(m); !strings.Contains(view, "too old to report its tools") {
		t.Fatalf("view should explain the missing fleetd rows:\n%s", view)
	}
}

func TestFetchDaemonTools(t *testing.T) {
	stubListDeployTargets(t, &fleetgrpc.ListDeployTargetsReply{
		Host:      "h",
		HostTools: []*fleetgrpc.HostTool{{Name: "docker", Binary: "docker", Found: true}},
	}, nil)
	got := fetchDaemonTools()
	if got.host != "h" || len(got.tools) != 1 || !got.tools[0].Found || got.err != "" {
		t.Fatalf("fetchDaemonTools() = %+v", got)
	}

	stubListDeployTargets(t, nil, grpcstatus.Error(grpccodes.Unimplemented, "unknown method"))
	if got := fetchDaemonTools(); got.err == "" || len(got.tools) != 0 {
		t.Fatalf("old daemon: %+v, want an explanation and no tools", got)
	}

	stubListDeployTargets(t, nil, errors.New("boom"))
	if got := fetchDaemonTools(); !strings.Contains(got.err, "boom") {
		t.Fatalf("error: %+v", got)
	}
}

// Every backend type has a label of its own: a new one must not show as a
// second "Devcontainer".
func TestBackendTypeLabelNamesEveryBackendType(t *testing.T) {
	seen := map[string]fleet.BackendType{}
	for _, backendType := range fleet.BackendTypes() {
		label := backendTypeLabel(backendType)
		if other, dup := seen[label]; dup {
			t.Errorf("%q and %q share the label %q", other, backendType, label)
		}
		seen[label] = backendType
	}
	if got := backendTypeLabel(""); got != "Devcontainer" {
		t.Errorf(`backendTypeLabel("") = %q, want Devcontainer`, got)
	}
}
