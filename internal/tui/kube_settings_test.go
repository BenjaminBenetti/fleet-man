package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/x/ansi"
)

// The Kubernetes section sits after Codespaces (before Browser): one row per
// pushed connection in the daemon's order, then the add button, which is
// there even before anything is connected.
func TestKubeSectionRowsFollowCodespaces(t *testing.T) {
	m, sp := kubeTestModel(t)
	items := sp.visibleItems(m)
	add := slices.Index(items, settingsItemKubeAdd)
	if add < 0 || items[add-1] != settingsItemCodespacesMachine || items[add+1] != settingsItemBrowserMultiple {
		t.Fatalf("+ Kubernetes Cluster must sit between Codespaces and Browser: %v", items)
	}

	m.kubeClusters.Clusters = []*fleetgrpc.KubeClusterStatus{
		kubeSt("b-1", "beta", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY),
		kubeSt("a-1", "alpha", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY),
	}
	items = sp.visibleItems(m)
	at := slices.Index(items, settingsItemKubeBase)
	if at < 0 || items[at+1] != settingsItemKubeBase+1 || items[at+2] != settingsItemKubeAdd {
		t.Fatalf("cluster rows must come first, in pushed order, then the add row: %v", items)
	}
	if !isKubeClusterItem(settingsItemKubeBase+1) || isKubeClusterItem(settingsItemKubeAdd) || isKubeClusterItem(settingsItemArmadaBase) {
		t.Fatal("isKubeClusterItem must cover only the 50000 band")
	}
}

// A push replaces the list; an open Settings page keeps its cursor on the
// same connection when rows move under it, and a push from a superseded
// connection (an armada switch) is dropped.
func TestKubeClustersPushKeepsTheCursor(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("b-1", "beta", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY))
	sp.cursor = settingsPositionOf(sp, m, settingsItemKubeBase)

	pushed := &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{
		kubeSt("a-1", "alpha", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING),
		kubeSt("b-1", "beta", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED),
	}}
	m.handleKubeClustersMsg(kubeClustersMsg{clusters: pushed, gen: 0})
	if got := sp.settingsCursorItem(m); got != settingsItemKubeBase+1 {
		t.Fatalf("cursor on item %d, want beta's row (now %d)", got, settingsItemKubeBase+1)
	}
	if m.kubeStatus("b-1").GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED {
		t.Fatal("the push must replace the list")
	}

	m.handleKubeClustersMsg(kubeClustersMsg{clusters: &fleetgrpc.KubeClusters{}, gen: 7})
	if len(m.kubeStatuses()) != 2 {
		t.Fatal("a push from another connection generation must be dropped")
	}
}

// A row draws health from the push: the badge, the version, what uses it,
// and every warning on its own line; a too-long reason is cut on the row and
// given in full below. The trust line names the RBAC verbs.
func TestKubeRowRendersHealthUsersAndWarnings(t *testing.T) {
	ready := kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	ready.Capabilities = &fleetgrpc.KubeCapabilities{Version: "v1.37.0"}
	ready.Warnings = []string{"privileged allowed"}
	expired := kubeSt("stg-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED)
	expired.Message = "credentials expired 2h ago"
	limited := kubeSt("eu-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED)
	limited.Message = "privileged denied · one node per fleet · " + strings.Repeat("long reason ", 10)
	m, sp := kubeTestModel(t, ready, expired, limited)
	m.st.Fleets["fm"] = &fleet.Fleet{Name: "fm", Instances: []*fleet.Instance{
		{Name: "a", KubeCluster: "mk-1", Status: fleet.StatusRunning},
		{Name: "b", KubeCluster: "mk-1", Status: fleet.StatusStopped},
	}, Settings: fleet.FleetSettings{Agents: []fleet.Agent{{Name: "nightly", KubeCluster: "stg-1"}}}}

	view := sp.viewSettings(m)
	out := flatText(view)
	for _, want := range []string{
		"Kubernetes contexts from fleetd @ ben-desktop",
		"☸ minikube", "minikube-ctx · fleet", "● ready v1.37.0", "2 instances", "[ test ] [ delete ]",
		"⚠ privileged allowed",
		"☸ staging", "✕ unauthorized (credentials expired 2h ago)", "1 agent",
		"☸ prod-eu", "▲ privileged denied",
		"+ Kubernetes Cluster",
		"pods/exec, pod create or statefulset patch in a namespace = inside your instances",
		"pod create or PVC / volume snapshot access there = your agent logins",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("settings view missing %q", want)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(line); w > m.width {
			t.Errorf("line wider than the terminal (%d > %d): %q", w, m.width, ansi.Strip(line))
		}
	}
	if !strings.Contains(out, strings.Join(strings.Fields(limited.Message), " ")) {
		t.Error("a reason cut on the row must be given in full below it")
	}
}

// l/→ steps a cluster row onto [ test ], then [ delete ]; h/← steps back,
// and moving to another row puts the sub-cursor back on the row. Enter on the
// row opens the editor, on [ delete ] the disconnect dialog.
func TestKubeRowButtonsRouteEnter(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY))
	sp.cursor = settingsPositionOf(sp, m, settingsItemKubeBase)

	press(m, sp, "l")
	if !sp.kubeTestFocused || sp.kubeDeleteFocused {
		t.Fatal("l must focus [ test ]")
	}
	press(m, sp, "right")
	if sp.kubeTestFocused || !sp.kubeDeleteFocused {
		t.Fatal("→ must step on to [ delete ]")
	}
	press(m, sp, "l")
	if !sp.kubeDeleteFocused {
		t.Fatal("[ delete ] is the last button")
	}
	if !strings.Contains(sp.kubeRowFooter(settingsItemKubeBase), "disconnect") {
		t.Fatalf("footer on [ delete ] = %q", sp.kubeRowFooter(settingsItemKubeBase))
	}
	press(m, sp, "h")
	if !sp.kubeTestFocused {
		t.Fatal("h must step back to [ test ]")
	}
	press(m, sp, "j")
	if sp.kubeTestFocused || sp.kubeDeleteFocused {
		t.Fatal("leaving the row must reset its sub-cursor")
	}
	press(m, sp, "k")

	press(m, sp, "enter")
	if _, ok := sp.kubeDialog.(*kubeEditor); !ok {
		t.Fatalf("enter on the row must open the cluster editor, got %T", sp.kubeDialog)
	}
	press(m, sp, "esc")
	if sp.kubeDialog != nil {
		t.Fatal("esc must close the editor")
	}
	press(m, sp, "l")
	press(m, sp, "l")
	press(m, sp, "enter")
	if _, ok := sp.kubeDialog.(*kubeRemoveDialog); !ok {
		t.Fatalf("enter on [ delete ] must open the disconnect dialog, got %T", sp.kubeDialog)
	}
}

// [ test ] streams a preflight of the saved connection: the row shows the
// checks as they come, then the result becomes its health and the message
// line says it.
func TestKubeRowTestStreamsChecks(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY))
	sp.cursor = settingsPositionOf(sp, m, settingsItemKubeBase)
	result := kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED)
	result.Message = "debug caps denied"
	reqs := stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(result, &fleetgrpc.KubeCheck{Name: "kubeconfig", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS},
			&fleetgrpc.KubeCheck{Name: "rbac", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING})
	})

	press(m, sp, "l")
	cmd := press(m, sp, "enter")
	run := m.kubeRuns[kubeTestKey("mk-1")]
	if run == nil || !run.running() {
		t.Fatal("[ test ] must start a run")
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "testing…") {
		t.Fatalf("a running test must show on the row:\n%s", out)
	}
	pumpKube(t, m, cmd)
	if len(*reqs) != 1 || (*reqs)[0].GetId() != "mk-1" || (*reqs)[0].GetCluster() != nil {
		t.Fatalf("requests = %v, want one test of the saved connection by id", *reqs)
	}
	if run.running() || len(run.checks) != 2 {
		t.Fatalf("run done=%v checks=%v", run.done, run.checks)
	}
	if m.kubeStatus("mk-1").GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED {
		t.Fatal("the result must become the connection's health at once")
	}
	if !strings.Contains(m.message, "limited (debug caps denied)") {
		t.Fatalf("message = %q", m.message)
	}
}

// The login plugins of the connections join the fleetd rows of Tool Status:
// one per connection with a plugin, missing while the connection is
// PLUGIN_MISSING, found once any later state proves fleetd ran it; enter
// opens its install page here.
func TestToolStatusLoginPluginRows(t *testing.T) {
	missing := kubeSt("eks-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING)
	found := kubeSt("gke-1", "prod", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED)
	static := kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	m, sp := kubeTestModel(t, missing, found, static)
	m.daemonTools.tools = []deps.ToolStatus{{Name: "docker", Binary: "docker", Found: true}}

	asked := stubKubeContexts(t, &fleetgrpc.ListKubeContextsReply{Host: "ben-desktop", Contexts: []*fleetgrpc.KubeContext{
		{Name: "staging-ctx", Plugin: "aws", PluginMissing: "aws not found"},
		{Name: "prod-ctx", Plugin: "/usr/lib/google-cloud-sdk/bin/gke-gcloud-auth-plugin", PluginMissing: "not on PATH"},
		{Name: "minikube-ctx"},
	}})
	pumpKube(t, m, loadKubePluginsCmd(m.watchGen, m.kubeStatuses()))
	if len(*asked) != 1 {
		t.Fatalf("one ListKubeContexts per set of kubeconfig files, got %d", len(*asked))
	}

	rows := daemonToolRows(m)
	if len(rows) != 3 || rows[1].Name != "aws" || rows[1].Found || rows[2].Name != "gke-gcloud-auth-plugin" || !rows[2].Found {
		t.Fatalf("rows = %+v: want docker, aws (missing), gke-gcloud-auth-plugin (found: the connection got past it)", rows)
	}
	if rows[2].InstallURL != kubePluginInstallURL("gke-gcloud-auth-plugin") || !strings.Contains(rows[1].Description, "☸ staging") {
		t.Fatalf("rows = %+v", rows)
	}
	out := flatText(sp.viewSettings(m))
	if !strings.Contains(out, "aws not found on fleetd @ ben-desktop · Logs in to ☸ staging.") {
		t.Fatalf("Tool Status missing the plugin row:\n%s", out)
	}

	var opened string
	orig := openURL
	openURL = func(url string) { opened = url }
	t.Cleanup(func() { openURL = orig })
	sp.cursor = settingsPositionOf(sp, m, settingsItemDaemonToolBase+1)
	press(m, sp, "enter")
	if opened != kubePluginInstallURL("aws") {
		t.Fatalf("enter opened %q, want the aws install page", opened)
	}
}

func TestKubePluginInstallURLs(t *testing.T) {
	for cmd, want := range map[string]string{
		"aws":                    "docs.aws.amazon.com",
		"/opt/bin/kubelogin":     "azure.github.io/kubelogin",
		"gke-gcloud-auth-plugin": "cloud.google.com/kubernetes-engine",
		"kubectl-oidc_login":     "int128/kubelogin",
		"some-sso-helper":        "kubernetes.io/docs/reference/access-authn-authz/authentication",
	} {
		if got := kubePluginInstallURL(cmd); !strings.Contains(got, want) {
			t.Errorf("kubePluginInstallURL(%q) = %q, want %s", cmd, got, want)
		}
	}
}

// An armada switch drops the left daemon's connections and stops the tests
// and logins still streaming from it.
func TestArmadaSwitchClearsKubeCaches(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://old.example.com/old")
	m := armadaTestModel(nil)
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{kubeSt("mk-1", "minikube", 2)}}
	cancelled := 0
	m.kubeRuns = map[string]*kubeTestRun{"cluster:mk-1": {cancel: func() { cancelled++ }}}
	m.kubeLogins = map[string]*kubeLoginRun{"mk-1": {cancel: func() { cancelled++ }}}
	m.kubePlugins = map[string]kubePlugin{"mk-1": {command: "aws"}}
	m.kubeHost = "old-host"

	m.switchArmada(armadaEntry{displayName: "local"})
	if m.kubeClusters != nil || m.kubeRuns != nil || m.kubeLogins != nil || m.kubePlugins != nil || m.kubeHost != "" {
		t.Fatal("an armada switch must clear the kube caches")
	}
	if cancelled != 2 {
		t.Fatalf("cancelled %d streams, want the test and the login", cancelled)
	}
}

// The first-run dependency check says where clusters connect and what they
// need where fleetd runs.
func TestDepsCheckNamesKubernetes(t *testing.T) {
	m := &model{width: 120}
	out := flatText(newDepsCheckPage(nil).View(m))
	if !strings.Contains(out, "Kubernetes clusters connect in Settings → Kubernetes, and need docker and the devcontainer CLI where fleetd runs.") {
		t.Fatalf("first-run check missing the Kubernetes line:\n%s", out)
	}
}

// A cluster deploy target is named after its connection.
func TestDeployTargetLabelNamesTheCluster(t *testing.T) {
	m, _ := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
	if got := deployTargetLabel(m, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "mk-1"}); got != "☸ minikube" {
		t.Fatalf("label = %q", got)
	}
	if got := deployTargetLabel(m, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "gone-1"}); got != "☸ gone-1" {
		t.Fatalf("label of an unknown connection = %q", got)
	}
	if got := deployTargetLabel(m, fleet.DeployTarget{Backend: fleet.BackendDevcontainer}); got != "Devcontainer" {
		t.Fatalf("label = %q", got)
	}
}
