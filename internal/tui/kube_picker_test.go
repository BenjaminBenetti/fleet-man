package tui

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// pickerContexts is a daemon host's kubeconfig as ListKubeContexts reads it.
func pickerContexts() *fleetgrpc.ListKubeContextsReply {
	return &fleetgrpc.ListKubeContextsReply{
		Host:            "ben-desktop",
		KubeconfigPaths: []string{"/home/ben/.kube/config"},
		Contexts: []*fleetgrpc.KubeContext{
			{Name: "kind-lab", Server: "https://127.0.0.1:41233", Namespace: "default"},
			{Name: "minikube", Server: "https://192.168.49.2:8443", Current: true, Auth: "exec-plugin", Plugin: "minikube-login"},
			{Name: "gke_acme_prod", Server: "https://34.90.12.7", Namespace: "fleet-ben", Connected: []string{"eu-1"}},
			{Name: "eks-stg", Server: "https://abcd.eks.amazonaws.com", Namespace: "dev", Plugin: "aws", PluginMissing: "aws not on fleetd's PATH"},
		},
	}
}

// openPicker opens the picker from the + row and answers its context list.
func openPicker(t *testing.T, m *model, sp *settingsPage) *kubePicker {
	t.Helper()
	sp.cursor = settingsPositionOf(sp, m, settingsItemKubeAdd)
	pumpKube(t, m, press(m, sp, "enter"))
	p, ok := sp.kubeDialog.(*kubePicker)
	if !ok {
		t.Fatalf("enter on + Kubernetes Cluster opened %T", sp.kubeDialog)
	}
	return p
}

// The picker shows the DAEMON host's contexts (fleetd's default kubeconfig),
// starts on the current context, marks connected ones and a login plugin
// fleetd lacks.
func TestPickerListsTheDaemonsContexts(t *testing.T) {
	m, sp := kubeTestModel(t)
	asked := stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	if len(*asked) != 1 || (*asked)[0] != nil {
		t.Fatalf("asked %v, want fleetd's defaults (nil paths)", *asked)
	}
	if p.selected().GetName() != "minikube" {
		t.Fatalf("cursor on %q, want the current context", p.selected().GetName())
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{
		"Connect a Kubernetes cluster", "contexts on fleetd @ ben-desktop · /home/ben/.kube/config",
		"> * minikube", "✓ gke_acme_prod", "exec: aws ⚠ aws not on fleetd's PATH",
		"namespace: fleet (suggested: the context's is default · n changes it)",
		"[/] filter", "[enter] test & connect", "[ctrl+o] other kubeconfig", "[L] login here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing %q in:\n%s", want, out)
		}
	}
	if m.kubeHost != "ben-desktop" {
		t.Fatal("the daemon's host name must be remembered for the section header")
	}
}

// A long host name and a long kubeconfig path do not share a shortened line:
// the path read gets a line of its own and is shown whole.
func TestPickerKeepsTheKubeconfigPathWhole(t *testing.T) {
	m, sp := kubeTestModel(t)
	reply := pickerContexts()
	host := "iad20-eo1207-d9f66e4f-d7df-4150-9b7e-3f0c2a1d5e6b"
	path := "/var/folders/36/tjdph2t965j8snz9_vkdnw0r0000gn/T/tmp.KIJJwVPMOj/kubeconfig"
	reply.Host, reply.KubeconfigPaths = host, []string{path}
	stubKubeContexts(t, reply)
	openPicker(t, m, sp)
	out := flatText(sp.viewSettings(m))
	if want := "contexts on fleetd @ " + host + " " + path + " Filter:"; !strings.Contains(out, want) {
		t.Fatalf("picker missing %q in:\n%s", want, out)
	}
}

// After /, typed letters filter: j, k, n, c and L do nothing else. esc
// leaves the filter (keeping it) and the keys work again.
func TestPickerFilterSwallowsLetters(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	called := false
	orig := kubeLoginHereCommandFn
	kubeLoginHereCommandFn = func(*fleetgrpc.KubeCluster) (*exec.Cmd, error) { called = true; return nil, errors.New("no") }
	t.Cleanup(func() { kubeLoginHereCommandFn = orig })

	press(m, sp, "/")
	for _, k := range []string{"k", "i", "n", "d", "L", "j", "c"} {
		press(m, sp, k)
	}
	if p.filter.Value() != "kindLjc" || !p.filtering || called || p.inputMode != kubePickerInputNone {
		t.Fatalf("filter=%q filtering=%v login=%v input=%v: typed letters must only filter", p.filter.Value(), p.filtering, called, p.inputMode)
	}
	if len(p.filtered()) != 0 {
		t.Fatalf("nothing matches %q", p.filter.Value())
	}
	for range "Ljc" {
		press(m, sp, "backspace")
	}
	if got := p.filtered(); len(got) != 1 || got[0].GetName() != "kind-lab" || p.cursor != 0 {
		t.Fatalf("filter %q matched %v (cursor %d)", p.filter.Value(), got, p.cursor)
	}
	press(m, sp, "esc")
	if p.filtering || p.filter.Value() != "kind" || sp.kubeDialog != p {
		t.Fatal("esc must leave the filter, keeping it, and not close the picker")
	}
	press(m, sp, "esc")
	if sp.kubeDialog != nil {
		t.Fatal("a second esc closes the picker")
	}
}

// Enter tests the selected context as a draft (nothing saved), streaming its
// checks, and connects it when it is usable: the draft pins the files read,
// takes "fleet" for a context on default, and is named after the context.
func TestPickerTestsThenConnects(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("x-1", "kind-lab", 2))
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	ready := kubeSt("", "", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	reqs := stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(ready, &fleetgrpc.KubeCheck{Name: "kubeconfig", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS})
	})
	var added *fleetgrpc.KubeCluster
	origAdd := addKubeClusterRemote
	addKubeClusterRemote = func(c *fleetgrpc.KubeCluster) (*fleetgrpc.KubeClusterStatus, error) {
		added = proto.Clone(c).(*fleetgrpc.KubeCluster)
		st := kubeSt("minikube-ab12cd", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING)
		return st, nil
	}
	t.Cleanup(func() { addKubeClusterRemote = origAdd })

	pumpKube(t, m, press(m, sp, "enter"))
	want := &fleetgrpc.KubeCluster{DisplayName: "minikube", Context: "minikube", KubeconfigPaths: []string{"/home/ben/.kube/config"}, Namespace: "fleet"}
	if len(*reqs) != 1 || !proto.Equal((*reqs)[0].GetCluster(), want) || (*reqs)[0].GetId() != "" {
		t.Fatalf("test requests = %v, want one draft test of %v", *reqs, want)
	}
	if !proto.Equal(added, want) {
		t.Fatalf("added %v, want %v", added, want)
	}
	if sp.kubeDialog != nil {
		t.Fatal("a connected context closes the picker")
	}
	if m.kubeStatus("minikube-ab12cd") == nil || sp.settingsCursorItem(m) != settingsItemKubeBase+1 {
		t.Fatalf("the new connection must show at once with the cursor on it (cursor %d)", sp.settingsCursorItem(m))
	}
	if !strings.Contains(m.message, "Connected ☸ minikube") {
		t.Fatalf("message = %q", m.message)
	}
	_ = p
}

// A context whose name is taken gets context/namespace as its name.
func TestPickerNamesAroundATakenName(t *testing.T) {
	m, _ := kubeTestModel(t, kubeSt("mk-1", "Minikube", 2))
	if got := kubeDefaultName(m, "minikube", "team"); got != "minikube/team" {
		t.Fatalf("name = %q", got)
	}
	if got := kubeDefaultName(m, "kind", "x"); got != "kind" {
		t.Fatalf("name = %q", got)
	}
}

// A failed test says why and offers the next step: for a missing namespace
// [c] Create, which shows the manifest from a dry run before creating it and
// then tests again; a second enter connects the failing context anyway.
func TestPickerFailedTestOffersCreateAndConnectAnyway(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	nsMissing := &fleetgrpc.KubeCheck{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL,
		Detail: "namespace 'fleet' does not exist", Fix: "create it"}
	failing := kubeSt("", "", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED)
	failing.Message = "namespace 'fleet' does not exist"
	reqs := stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(failing, nsMissing)
	})
	var nsReqs []*fleetgrpc.CreateKubeNamespaceRequest
	origNS := createKubeNamespaceRemote
	createKubeNamespaceRemote = func(req *fleetgrpc.CreateKubeNamespaceRequest) (*fleetgrpc.CreateKubeNamespaceReply, error) {
		nsReqs = append(nsReqs, proto.Clone(req).(*fleetgrpc.CreateKubeNamespaceRequest))
		return &fleetgrpc.CreateKubeNamespaceReply{Manifest: "apiVersion: v1\nkind: Namespace\nmetadata:\n  labels:\n    app.kubernetes.io/managed-by: fleet\n  name: fleet\n", Created: !req.GetDryRun()}, nil
	}
	t.Cleanup(func() { createKubeNamespaceRemote = origNS })
	addCalls := 0
	origAdd := addKubeClusterRemote
	addKubeClusterRemote = func(c *fleetgrpc.KubeCluster) (*fleetgrpc.KubeClusterStatus, error) {
		addCalls++
		return kubeSt("minikube-1", c.GetDisplayName(), 1), nil
	}
	t.Cleanup(func() { addKubeClusterRemote = origAdd })

	pumpKube(t, m, press(m, sp, "enter"))
	if addCalls != 0 || !p.failed {
		t.Fatal("an unusable draft must not connect on its own")
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{"✕ misconfigured", "namespace 'fleet' does not exist · create it", "[c] Create (shows labels)", "[enter] connect anyway", "[c] create namespace"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing %q:\n%s", want, out)
		}
	}

	pumpKube(t, m, press(m, sp, "c"))
	if p.ns == nil || len(nsReqs) != 1 || !nsReqs[0].GetDryRun() || nsReqs[0].GetCluster().GetContext() != "minikube" || nsReqs[0].GetNamespace() != "fleet" {
		t.Fatalf("c must dry-run the draft's namespace first: %v", nsReqs)
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "app.kubernetes.io/managed-by: fleet") || !strings.Contains(out, "[enter] create") {
		t.Fatalf("the panel must show the manifest, labels included:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(nsReqs) != 2 || nsReqs[1].GetDryRun() || p.ns != nil {
		t.Fatalf("enter must create it and close the panel: %v", nsReqs)
	}
	if len(*reqs) != 2 {
		t.Fatalf("a created namespace must test the context again (%d tests)", len(*reqs))
	}

	pumpKube(t, m, press(m, sp, "enter"))
	if addCalls != 1 {
		t.Fatal("enter after a failed test must connect anyway")
	}
}

// n chooses the namespace; ctrl+o reads other kubeconfig files on the
// daemon host (any absolute paths, as locally), separated like $KUBECONFIG.
func TestPickerNamespaceAndOtherKubeconfig(t *testing.T) {
	m, sp := kubeTestModel(t)
	asked := stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)

	press(m, sp, "n")
	if p.inputMode != kubePickerInputNamespace || p.input.Value() != "fleet" {
		t.Fatalf("n must edit the namespace, starting from the suggestion (%q)", p.input.Value())
	}
	p.input.SetValue("team-a")
	press(m, sp, "enter")
	if d := p.draft(m, p.selected()); d.GetNamespace() != "team-a" {
		t.Fatalf("draft namespace = %q", d.GetNamespace())
	}

	press(m, sp, "ctrl+o")
	if p.inputMode != kubePickerInputPaths || p.input.Value() != "/home/ben/.kube/config" {
		t.Fatalf("ctrl+o must edit the kubeconfig files (%q)", p.input.Value())
	}
	p.input.SetValue("/srv/kube/a.yaml : /srv/kube/b.yaml")
	pumpKube(t, m, press(m, sp, "enter"))
	if got := (*asked)[len(*asked)-1]; !slices.Equal(got, []string{"/srv/kube/a.yaml", "/srv/kube/b.yaml"}) {
		t.Fatalf("listed %v, want both files", got)
	}
}

// [L] Run login here runs the plugin in this terminal, so it is offered only
// over the local socket: a remote TUI (gateway or Armada SSH) neither shows
// nor runs it.
func TestPickerLoginHereOnlyOverTheLocalSocket(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
		offered          bool
	}{
		{"local", "", "", true},
		{"gateway", fleetclient.EnvGateway, "https://gw.example/s", false},
		{"armada ssh", fleetclient.EnvSSH, "ssh://ben@desk", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sp := kubeTestModel(t)
			if tc.env != "" {
				t.Setenv(tc.env, tc.value)
			}
			stubKubeContexts(t, pickerContexts())
			openPicker(t, m, sp)
			var ran *fleetgrpc.KubeCluster
			orig := kubeLoginHereCommandFn
			kubeLoginHereCommandFn = func(c *fleetgrpc.KubeCluster) (*exec.Cmd, error) { ran = c; return nil, errors.New("stub") }
			t.Cleanup(func() { kubeLoginHereCommandFn = orig })

			pumpKube(t, m, press(m, sp, "L"))
			if (ran != nil) != tc.offered {
				t.Fatalf("L ran=%v, want %v", ran != nil, tc.offered)
			}
			if got := strings.Contains(flatText(sp.viewSettings(m)), "[L] login here"); got != tc.offered {
				t.Fatalf("[L] in the footer = %v, want %v", got, tc.offered)
			}
			if tc.offered && (ran.GetContext() != "minikube" || !slices.Equal(ran.GetKubeconfigPaths(), []string{"/home/ben/.kube/config"})) {
				t.Fatalf("login here ran for %v", ran)
			}
			// A context without a login plugin has no login to run.
			ran = nil
			press(m, sp, "k") // kind-lab: a plain context
			if pumpKube(t, m, press(m, sp, "L")); ran != nil || strings.Contains(flatText(sp.viewSettings(m)), "[L] login here") {
				t.Fatal("L must not be offered for a context without a login plugin")
			}
		})
	}
}

// Re-point (from the editor's Context row) moves the connection to the
// picked context in one masked update, and the daemon's refusal (another
// cluster) is shown, not swallowed.
func TestPickerRepointsAConnection(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("eu-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING))
	stubKubeContexts(t, pickerContexts())
	calls := stubKubeUpdate(t, func(c kubeUpdateCall) (*fleetgrpc.KubeClusterStatus, error) {
		if c.cluster.GetContext() == "kind-lab" {
			return nil, status.Error(codes.FailedPrecondition, `context "kind-lab" reaches a different cluster`)
		}
		st := kubeSt("eu-1", "prod-eu", 1)
		st.Cluster.Context, st.Cluster.Revision = c.cluster.GetContext(), 4
		return st, nil
	})
	editor := newKubeEditor("eu-1")
	sp.kubeDialog = editor
	editor.row = 1 // Context
	pumpKube(t, m, press(m, sp, "enter"))
	p, ok := sp.kubeDialog.(*kubePicker)
	if !ok || p.repoint != "eu-1" {
		t.Fatalf("enter on Context must open the picker on a re-point, got %T", sp.kubeDialog)
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Re-point ☸ prod-eu") || !strings.Contains(out, "[enter] re-point here") {
		t.Fatalf("re-point picker:\n%s", out)
	}

	p.cursor = 0 // kind-lab
	pumpKube(t, m, press(m, sp, "enter"))
	if !strings.Contains(p.err, "reaches a different cluster") || sp.kubeDialog != p {
		t.Fatalf("a refused re-point must say why and stay open (err %q)", p.err)
	}
	p.cursor = 2 // gke_acme_prod
	pumpKube(t, m, press(m, sp, "enter"))
	last := (*calls)[len(*calls)-1]
	if last.id != "eu-1" || !slices.Equal(last.paths, []string{"context", "kubeconfig_paths"}) || last.rev != 3 ||
		last.cluster.GetContext() != "gke_acme_prod" || !slices.Equal(last.cluster.GetKubeconfigPaths(), []string{"/home/ben/.kube/config"}) {
		t.Fatalf("re-point update = %+v", last)
	}
	if sp.kubeDialog != editor || m.kubeStatus("eu-1").GetCluster().GetContext() != "gke_acme_prod" {
		t.Fatalf("a re-point returns to the editor with the new context (dialog %T)", sp.kubeDialog)
	}
}

// A long context (an EKS ARN) still names the connection: shortened in the
// middle to the daemon's 63-byte limit (the ellipsis counts three bytes),
// keeping the cluster name at the end. A name the daemon would refuse
// outright is left for it to choose.
func TestPickerNamesALongContextWithinTheDaemonLimit(t *testing.T) {
	m, _ := kubeTestModel(t)
	arn := "arn:aws:eks:ap-southeast-2:123456789012:cluster/acme-production-platform"
	got := kubeDefaultName(m, arn, "fleet")
	if len(got) > kubeNameMaxBytes || !strings.HasPrefix(got, "arn:aws:eks:") || !strings.HasSuffix(got, "production-platform") || !strings.Contains(got, "…") {
		t.Fatalf("name = %q (%d bytes)", got, len(got))
	}
	wide := strings.Repeat("ü", 40) + "-cluster"
	if got := kubeDefaultName(m, wide, "fleet"); len(got) > kubeNameMaxBytes || !strings.HasSuffix(got, "-cluster") {
		t.Fatalf("name = %q (%d bytes)", got, len(got))
	}
	if got := kubeDefaultName(m, "ctx\x07bell", "fleet"); got != "" {
		t.Fatalf("name = %q, want the daemon to name it", got)
	}
}

// What connects is what was tested: a namespace chosen with n while the test
// ran waits for its own test instead of connecting untested.
func TestPickerConnectsOnlyTheTestedNamespace(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	ready := kubeSt("", "", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	reqs := stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(ready)
	})
	var added []*fleetgrpc.KubeCluster
	origAdd := addKubeClusterRemote
	addKubeClusterRemote = func(c *fleetgrpc.KubeCluster) (*fleetgrpc.KubeClusterStatus, error) {
		added = append(added, proto.Clone(c).(*fleetgrpc.KubeCluster))
		return kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING), nil
	}
	t.Cleanup(func() { addKubeClusterRemote = origAdd })

	test := press(m, sp, "enter") // the test of namespace fleet, not answered yet
	press(m, sp, "n")
	p.input.SetValue("team-a")
	press(m, sp, "enter")
	pumpKube(t, m, test)
	if len(added) != 0 || sp.kubeDialog != p {
		t.Fatalf("added %v: team-a was never tested", added)
	}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 2 || (*reqs)[1].GetCluster().GetNamespace() != "team-a" || len(added) != 1 || added[0].GetNamespace() != "team-a" {
		t.Fatalf("tests %v, added %v: enter must test team-a, then connect it", *reqs, added)
	}
}
