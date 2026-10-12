package tui

import (
	"errors"
	"fmt"
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

// draftResult is a draft test's result as the daemon reports it for req on
// a cluster of nodes: a node-local default class, a network one Auto picks,
// a file share, and delivery Auto resolves to the registry (the gateway
// serves no images).
func draftResult(req *fleetgrpc.TestKubeClusterRequest, state fleetgrpc.KubeClusterState, nodes int) *fleetgrpc.KubeClusterStatus {
	c := proto.Clone(req.GetCluster()).(*fleetgrpc.KubeCluster)
	caps := &fleetgrpc.KubeCapabilities{
		StorageClasses: []*fleetgrpc.KubeStorageClass{
			{Name: "efs", Provisioner: "efs.csi.aws.com", DiskHint: "an NFS class for instance disks: root_squash breaks package installs"},
			{Name: "gp3", Provisioner: "ebs.csi.aws.com", Block: true, AllowExpansion: true},
			{Name: "local-path", Provisioner: "rancher.io/local-path", Block: true, NodeLocal: true, IsDefault: true},
		},
		InstanceStorageClass: c.GetStorageClass(),
		ImageDelivery:        &fleetgrpc.KubeImageDelivery{Mode: c.GetImage().GetMode(), GatewayReason: "fleetd has no gateway"},
	}
	if caps.InstanceStorageClass == "" {
		caps.InstanceStorageClass = "gp3"
	}
	if caps.ImageDelivery.Mode == "" {
		caps.ImageDelivery.Mode, caps.ImageDelivery.Reason = "registry", "a registry is set"
	}
	for i := range nodes {
		caps.Nodes = append(caps.Nodes, &fleetgrpc.KubeNode{Name: fmt.Sprintf("node-%d", i)})
	}
	return &fleetgrpc.KubeClusterStatus{Cluster: c, State: state, Capabilities: caps}
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

// pickerAt puts the cursor on a form field.
func pickerAt(t *testing.T, p *kubePicker, field string) {
	t.Helper()
	for i, row := range p.rows() {
		if row.field == field {
			p.cursor = i
			return
		}
	}
	t.Fatalf("no form row %q in %v", field, p.rows())
}

// The picker shows the DAEMON host's contexts (fleetd's default kubeconfig),
// starts on the current context, marks connected ones and a login plugin
// fleetd lacks, and says what enter does.
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
		"enter on a context sets up its connection here: name, namespace, image delivery, storage",
		"[/] filter", "[enter] choose & test", "[ctrl+o] other kubeconfig", "[L] login here",
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

// enter on a context chooses it and tests it as a draft (nothing saved),
// which fills the form's choices: the cursor waits on [ Connect ] once the
// test says it is usable, and enter there connects exactly what was tested.
// The draft pins the files read, takes "fleet" for a context on default,
// and is named after the context.
func TestPickerChoosesTestsThenConnects(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("x-1", "kind-lab", 2))
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	reqs := stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, 1),
			&fleetgrpc.KubeCheck{Name: "kubeconfig", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS},
			&fleetgrpc.KubeCheck{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: "fleet exists"})
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-ab12cd", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING)}, nil
	})

	pumpKube(t, m, press(m, sp, "enter"))
	want := &fleetgrpc.KubeCluster{DisplayName: "minikube", Context: "minikube", KubeconfigPaths: []string{"/home/ben/.kube/config"}, Namespace: "fleet"}
	if len(*reqs) != 1 || !proto.Equal((*reqs)[0].GetCluster(), want) || (*reqs)[0].GetId() != "" {
		t.Fatalf("test requests = %v, want one draft test of %v", *reqs, want)
	}
	if len(*added) != 0 || p.chosen != "minikube" || p.current().field != kubeFormConnect {
		t.Fatalf("choosing must test only, with the cursor on the button (added %v, row %+v)", *added, p.current())
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{
		"New connection ☸ minikube", "Name: minikube", "Namespace: fleet the context's is default ✓ exists",
		"Delivery: [Auto→Registry] [Node load] [Registry] [Gateway]", "Storage: [ auto → gp3 ] network · expandable",
		"> [ Connect ]", "● ready", "Caches, resources, security and the rest: the cluster editor, once connected",
		"[enter] connect",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("form missing %q in:\n%s", want, out)
		}
	}

	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 1 || len(*added) != 1 || !proto.Equal((*added)[0], want) {
		t.Fatalf("added %v (after %d tests), want %v once, untested again", *added, len(*reqs), want)
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
}

// The form's rows are reached with j/k (and tab) and edited in place: the
// name without a test, the namespace, delivery (Registry adds its row;
// Gateway is greyed out while the gateway serves no images), storage
// (Auto, then each class the test listed; a node-local one on several
// nodes asks for Node-local OK) each tested again, and the connection is
// made with all of it.
func TestPickerFormSetsTheBasicConfig(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	reqs := stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, 3))
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING)}, nil
	})
	pumpKube(t, m, press(m, sp, "enter"))

	// tab jumps between the chosen context and the form; j/k walk on.
	press(m, sp, "tab")
	if p.selected().GetName() != "minikube" {
		t.Fatalf("tab on the form must come back to the chosen context, at %+v", p.current())
	}
	press(m, sp, "tab")
	if p.current().field != kubeFormName {
		t.Fatalf("tab on a context must jump to the form's first row, at %+v", p.current())
	}
	press(m, sp, "tab")
	press(m, sp, "j")
	press(m, sp, "j")
	press(m, sp, "j")
	if p.current().field != kubeFormName {
		t.Fatalf("j from the last context must reach the form, at %+v", p.current())
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "[j/k] [enter] edit [tab] contexts") {
		t.Errorf("a text row's footer:\n%s", out)
	}

	press(m, sp, "enter")
	p.input.SetValue("lab")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 1 || p.draft(m, p.chosenContext()).GetDisplayName() != "lab" {
		t.Fatalf("the name must change without a test (%d tests, name %q)", len(*reqs), p.draft(m, p.chosenContext()).GetDisplayName())
	}

	press(m, sp, "j")
	press(m, sp, "enter")
	if p.inputMode != kubePickerInputField || p.input.Value() != "fleet" {
		t.Fatalf("enter on Namespace must edit it from the suggestion (%q)", p.input.Value())
	}
	p.input.SetValue("team-a")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 2 || (*reqs)[1].GetCluster().GetNamespace() != "team-a" {
		t.Fatalf("a new namespace must be tested: %v", *reqs)
	}

	press(m, sp, "j")
	if p.current().field != kubeFormDelivery {
		t.Fatalf("at %+v, want Delivery", p.current())
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Auto picked Registry: a registry is set") || !strings.Contains(out, "Gateway unavailable: fleetd has no gateway") ||
		!strings.Contains(out, "Node load unavailable: the nodes are not a local cluster") || !strings.Contains(out, "[←/→] change") {
		t.Errorf("Delivery's lines:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "l"))
	if p.form(p.chosenContext()).mode != kubeDeliveryRegistry || !slices.Contains(p.formFields(p.chosenContext()), kubeFormRegistry) {
		t.Fatalf("→ must step over the greyed-out Node load to Registry, and add its row: delivery = %q, fields %v",
			p.form(p.chosenContext()).mode, p.formFields(p.chosenContext()))
	}
	pumpKube(t, m, press(m, sp, "l"))
	if p.form(p.chosenContext()).mode != kubeDeliveryAuto {
		t.Fatalf("→ must step over the greyed-out Gateway back to Auto, at %q", p.form(p.chosenContext()).mode)
	}
	pumpKube(t, m, press(m, sp, "h"))
	press(m, sp, "j")
	press(m, sp, "enter")
	p.input.SetValue("ghcr.io/ben/fleet")
	pumpKube(t, m, press(m, sp, "enter"))

	press(m, sp, "j")
	if p.current().field != kubeFormStorage {
		t.Fatalf("at %+v, want Storage", p.current())
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Storage: [ auto → gp3 ] network · expandable") ||
		!strings.Contains(out, "Storage: [ auto → gp3 ] network · expandable instance disks") {
		t.Errorf("Storage's lines:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "l"))
	if got := p.form(p.chosenContext()).storage; got != "efs" {
		t.Fatalf("storage = %q, want the first listed class", got)
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Storage: [ efs ] file share") || !strings.Contains(out, "⚠ an NFS class for instance disks") {
		t.Errorf("a file-share class's lines:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "l"))
	pumpKube(t, m, press(m, sp, "l"))
	if got := p.form(p.chosenContext()).storage; got != "local-path" || !slices.Contains(p.formFields(p.chosenContext()), kubeFormNodeLocal) {
		t.Fatalf("storage = %q, fields %v: a node-local class on 3 nodes asks for Node-local OK", got, p.formFields(p.chosenContext()))
	}
	press(m, sp, "j")
	pumpKube(t, m, press(m, sp, "enter"))
	if !p.form(p.chosenContext()).nodeLocalAck {
		t.Fatal("enter on Node-local OK must turn it on")
	}

	press(m, sp, "j")
	if p.current().field != kubeFormConnect {
		t.Fatalf("at %+v, want the button", p.current())
	}
	pumpKube(t, m, press(m, sp, "enter"))
	want := &fleetgrpc.KubeCluster{DisplayName: "lab", Context: "minikube", KubeconfigPaths: []string{"/home/ben/.kube/config"}, Namespace: "team-a",
		Image: &fleetgrpc.KubeImageSettings{Mode: "registry", Registry: "ghcr.io/ben/fleet"}, StorageClass: "local-path", NodeLocalAck: true}
	if len(*added) != 1 || !proto.Equal((*added)[0], want) {
		t.Fatalf("added %v, want %v", *added, want)
	}
	if last := (*reqs)[len(*reqs)-1].GetCluster(); kubeDraftKey(last) != kubeDraftKey(want) {
		t.Fatalf("connected %v, last tested %v: only what was tested connects", want, last)
	}
}

// A draft whose only failure is a namespace that does not exist connects
// without a "connect anyway": the daemon creates the namespace as it adds
// the connection, and the message says so.
func TestPickerMissingNamespaceIsNoFailure(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		res := draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, 1)
		res.Message = "namespace 'fleet' does not exist"
		return testEvents(res,
			&fleetgrpc.KubeCheck{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "namespace 'fleet' does not exist", Fix: "fleet creates it when it connects the cluster"},
			&fleetgrpc.KubeCheck{Name: "rbac", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: "namespace missing"})
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING),
			Namespace: &fleetgrpc.KubeNamespaceResult{Namespace: "fleet", Created: true}}, nil
	})

	pumpKube(t, m, press(m, sp, "enter"))
	if p.failed {
		t.Fatal("a namespace to create is not a failed test")
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{"Namespace: fleet the context's is default does not exist: fleet creates it", "namespace 'fleet' does not exist: fleet creates it when it connects", "> [ Connect ]"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "connect anyway") {
		t.Errorf("no connect anyway for a namespace fleet creates:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*added) != 1 || sp.kubeDialog != nil || !strings.Contains(m.message, "created namespace fleet") {
		t.Fatalf("added %v, dialog %T, message %q", *added, sp.kubeDialog, m.message)
	}
}

// [ Test & connect ] waits for the test: a usable result connects at once.
func TestPickerTestAndConnectWaitsForTheTest(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, 1))
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING)}, nil
	})
	test := press(m, sp, "enter") // chosen; its test not answered yet
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "> [ Test & connect ]") {
		t.Fatalf("while the test runs the button tests & connects:\n%s", out)
	}
	press(m, sp, "enter")
	if !p.connect || len(*added) != 0 {
		t.Fatal("enter while the test runs must wait for it")
	}
	pumpKube(t, m, test)
	if len(*added) != 1 {
		t.Fatalf("a limited (usable) result must connect what waits for it: added %v", *added)
	}
}

// A failed test says why and offers to connect anyway; the connection's
// editor (Settings) is where it is fixed.
func TestPickerFailedTestConnectsAnyway(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		res := &fleetgrpc.KubeClusterStatus{Cluster: req.GetCluster(), State: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE, Message: "connection refused"}
		return testEvents(res, &fleetgrpc.KubeCheck{Name: "reachable", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "dial tcp 127.0.0.1:9: connection refused"})
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE)}, nil
	})
	pumpKube(t, m, press(m, sp, "enter"))
	if !p.failed || len(*added) != 0 {
		t.Fatal("an unusable draft must not connect on its own")
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{"✕ unreachable", "dial tcp 127.0.0.1:9: connection refused", "> [ Connect anyway ]", "[enter] connect anyway",
		"Storage: [ auto ] none listed: enter types a class name"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing %q:\n%s", want, out)
		}
	}
	press(m, sp, "k")
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "connect anyway with [ Connect anyway ]") {
		t.Errorf("off the button, the hint names it:\n%s", out)
	}
	press(m, sp, "j")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*added) != 1 {
		t.Fatal("enter on [ Connect anyway ] must connect")
	}
}

// When fleetd may not create the namespace, the connection is still made,
// and its editor opens on the namespace panel with the manifest to apply.
func TestPickerRefusedNamespaceOpensTheEditor(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	openPicker(t, m, sp)
	stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, 1),
			&fleetgrpc.KubeCheck{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "namespace 'fleet' does not exist"})
	})
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  labels:\n    app.kubernetes.io/managed-by: fleet\n  name: fleet\n"
	stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING),
			Namespace: &fleetgrpc.KubeNamespaceResult{Namespace: "fleet", PermissionDenied: true, Manifest: manifest,
				Error: `creating namespace "fleet": forbidden (it needs namespaces create; apply the manifest as someone who has it)`}}, nil
	})
	pumpKube(t, m, press(m, sp, "enter"))
	pumpKube(t, m, press(m, sp, "enter"))
	e, ok := sp.kubeDialog.(*kubeEditor)
	if !ok || e.id != "minikube-1" || e.ns == nil || !e.ns.refused || e.ns.manifest != manifest {
		t.Fatalf("a refused namespace must open the new connection's editor on its manifest, got %T", sp.kubeDialog)
	}
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{"Create namespace fleet", "apply this as someone who may create namespaces, or enter to try again",
		"app.kubernetes.io/managed-by: fleet", "it needs namespaces create", "y copies the manifest"} {
		if !strings.Contains(out, want) {
			t.Errorf("editor missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(m.message, "fleet may not create namespace fleet") {
		t.Errorf("message = %q", m.message)
	}
}

// n (kept from before the form) chooses the context under the cursor and
// edits its namespace; ctrl+o reads other kubeconfig files on the daemon
// host (any absolute paths, as locally), separated like $KUBECONFIG.
func TestPickerNamespaceKeyAndOtherKubeconfig(t *testing.T) {
	m, sp := kubeTestModel(t)
	asked := stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	reqs := stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, 1))
	})

	press(m, sp, "n")
	if p.chosen != "minikube" || p.current().field != kubeFormNamespace || p.inputMode != kubePickerInputField || p.input.Value() != "fleet" {
		t.Fatalf("n must choose minikube and edit its namespace from the suggestion (%q, %q)", p.chosen, p.input.Value())
	}
	p.input.SetValue("team-a")
	pumpKube(t, m, press(m, sp, "enter"))
	if d := p.draft(m, p.chosenContext()); d.GetNamespace() != "team-a" || len(*reqs) != 1 || (*reqs)[0].GetCluster().GetNamespace() != "team-a" {
		t.Fatalf("draft namespace = %q, tests %v", d.GetNamespace(), *reqs)
	}

	press(m, sp, "k")
	press(m, sp, "k") // the last context row
	press(m, sp, "ctrl+o")
	if p.inputMode != kubePickerInputPaths || p.input.Value() != "/home/ben/.kube/config" {
		t.Fatalf("ctrl+o must edit the kubeconfig files (%q)", p.input.Value())
	}
	p.input.SetValue("/srv/kube/a.yaml : /srv/kube/b.yaml")
	pumpKube(t, m, press(m, sp, "enter"))
	if got := (*asked)[len(*asked)-1]; !slices.Equal(got, []string{"/srv/kube/a.yaml", "/srv/kube/b.yaml"}) {
		t.Fatalf("listed %v, want both files", got)
	}
	if p.chosen != "" || len(p.forms) != 0 {
		t.Fatal("other files are other contexts: the form starts over")
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
// picked context in one masked update, without the form, and the daemon's
// refusal (another cluster) is shown, not swallowed.
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
	out := flatText(sp.viewSettings(m))
	if !strings.Contains(out, "Re-point ☸ prod-eu") || !strings.Contains(out, "[enter] re-point here") || strings.Contains(out, "New connection") || strings.Contains(out, "enter on a context sets up") {
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

// What connects is what was tested: a namespace set while [ Test &
// connect ] waited is tested on its own, and waits for enter again.
func TestPickerConnectsOnlyTheTestedDraft(t *testing.T) {
	m, sp := kubeTestModel(t)
	stubKubeContexts(t, pickerContexts())
	p := openPicker(t, m, sp)
	reqs := stubKubeTest(t, func(req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(draftResult(req, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, 1))
	})
	added := stubKubeAdd(t, func(c *fleetgrpc.KubeCluster) (*fleetgrpc.AddKubeClusterReply, error) {
		return &fleetgrpc.AddKubeClusterReply{Cluster: kubeSt("minikube-1", c.GetDisplayName(), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING)}, nil
	})

	test := press(m, sp, "enter") // the test of namespace fleet, not answered yet
	press(m, sp, "enter")         // [ Test & connect ]: waits for it
	press(m, sp, "n")
	p.input.SetValue("team-a")
	retest := press(m, sp, "enter")
	pumpKube(t, m, test)
	pumpKube(t, m, retest)
	if len(*added) != 0 || sp.kubeDialog != p {
		t.Fatalf("added %v: a change while waiting is tested, not connected", *added)
	}
	pickerAt(t, p, kubeFormConnect)
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 2 || (*reqs)[1].GetCluster().GetNamespace() != "team-a" || len(*added) != 1 || (*added)[0].GetNamespace() != "team-a" {
		t.Fatalf("tests %v, added %v: enter must connect the tested team-a", *reqs, *added)
	}
}
