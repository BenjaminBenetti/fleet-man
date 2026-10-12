//go:build !nok8s

package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fleet_cluster_list returns every connection with its health and settings,
// and one connection when named by id, name or context.
func TestMCPClusterList(t *testing.T) {
	f := newKubeFixture(t)
	extra := t.TempDir()
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet", ExtraPath: []string{extra}, AllowPrivileged: ptr(false)}).GetCluster()
	prod := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster()
	cs := mcpConnect(t, f.svc)

	var all FleetClusterListOutput
	callJSON(t, cs, "fleet_cluster_list", map[string]any{}, &all)
	if len(all.Clusters) != 2 {
		t.Fatalf("clusters = %+v", all.Clusters)
	}
	byID := map[string]mcpKubeCluster{}
	for _, c := range all.Clusters {
		byID[c.ID] = c
	}
	got := byID[dev.GetId()]
	if got.Name != "dev" || got.Context != "minikube" || got.Namespace != "fleet" || got.State != "checking" || got.Usable ||
		len(got.KubeconfigPaths) != 1 || got.KubeconfigPaths[0] != f.kubeconfig {
		t.Errorf("dev = %+v", got)
	}
	if got.Settings["allow_privileged"] != false || got.Settings["display_name"] != "dev" {
		t.Errorf("dev settings = %v", got.Settings)
	}
	if paths, _ := got.Settings["extra_path"].([]any); len(paths) != 1 || paths[0] != extra {
		t.Errorf("dev extra_path = %v", got.Settings["extra_path"])
	}
	if p := byID[prod.GetId()]; p.Name != prod.GetId() || p.Namespace != "team" {
		t.Errorf("an unnamed connection = %+v", p)
	}

	for ref, want := range map[string]string{"dev": dev.GetId(), dev.GetId(): dev.GetId(), "prod": prod.GetId()} {
		var one FleetClusterListOutput
		callJSON(t, cs, "fleet_cluster_list", map[string]any{"cluster": ref}, &one)
		if len(one.Clusters) != 1 || one.Clusters[0].ID != want {
			t.Errorf("fleet_cluster_list %q = %+v, want %s", ref, one.Clusters, want)
		}
	}
	if msg := callErr(t, cs, "fleet_cluster_list", map[string]any{"cluster": "nope"}); !strings.Contains(msg, `no Kubernetes connection "nope"`) {
		t.Errorf("unknown cluster: %q", msg)
	}
}

// fleet_cluster_test runs the full preflight and returns its checks and the
// resulting health, capabilities included; no output carries a credential.
func TestMCPClusterTest(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster()
	f.add(&fleetgrpc.KubeCluster{Context: "prod"})
	f.add(&fleetgrpc.KubeCluster{Context: "token-ctx", DisplayName: "gone"})
	cs := mcpConnect(t, f.svc)

	var out FleetClusterTestOutput
	callJSON(t, cs, "fleet_cluster_test", map[string]any{"cluster": "dev"}, &out)
	if out.Cluster.ID != dev.GetId() || out.Cluster.State != "ready" || !out.Cluster.Usable || out.Cluster.CheckedAt == "" {
		t.Fatalf("test result = %+v", out.Cluster)
	}
	if out.Cluster.Capabilities["version"] != "v1.37.0" {
		t.Errorf("capabilities = %v", out.Cluster.Capabilities)
	}
	names := map[string]string{}
	for _, c := range out.Checks {
		names[c.Name] = c.Result
	}
	for _, want := range []string{"kubeconfig", "reachable", "rbac", "admission_unprivileged"} {
		if names[want] != "pass" {
			t.Errorf("check %s = %q; checks %+v", want, names[want], out.Checks)
		}
	}
	if out.Cluster.DeepCheckedAt != "" {
		t.Errorf("a test without deep ran the deep test: deep_checked_at %s", out.Cluster.DeepCheckedAt)
	}

	// deep also runs the deep test's throwaway-pod probes.
	callJSON(t, cs, "fleet_cluster_test", map[string]any{"cluster": "dev", "deep": true}, &out)
	if out.Cluster.ID != dev.GetId() || out.Cluster.DeepCheckedAt == "" {
		t.Errorf("deep test result = %+v", out.Cluster)
	}

	callJSON(t, cs, "fleet_cluster_test", map[string]any{"cluster": "prod"}, &out)
	if out.Cluster.State != "ready" || !out.Cluster.Usable {
		t.Errorf("prod = %s usable %v: %q", out.Cluster.State, out.Cluster.Usable, out.Cluster.Message)
	}

	// The token user's cluster is gone from the fakes: a failed test is a
	// result, not a tool error, and it never carries the token.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fleet_cluster_test", Arguments: map[string]any{"cluster": "gone"}})
	if err != nil || res.IsError {
		t.Fatalf("test of a broken connection: %v %s", err, toolText(res))
	}
	if strings.Contains(toolText(res), fakeKubeToken) {
		t.Error("fleet_cluster_test output carries the kubeconfig token")
	}
	if err := json.Unmarshal([]byte(toolText(res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Cluster.Usable || out.Cluster.State != "context missing" || out.Cluster.Message == "" {
		t.Errorf("broken connection = %+v", out.Cluster)
	}

	var list FleetClusterListOutput
	callJSON(t, cs, "fleet_cluster_list", map[string]any{}, &list)
	data, _ := json.Marshal(list)
	if strings.Contains(string(data), fakeKubeToken) {
		t.Error("fleet_cluster_list output carries the kubeconfig token")
	}

	if msg := callErr(t, cs, "fleet_cluster_test", map[string]any{"cluster": "nope"}); !strings.Contains(msg, `no Kubernetes connection "nope"`) {
		t.Errorf("unknown cluster: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_cluster_test", map[string]any{"cluster": ""}); !strings.Contains(msg, "cluster is required") {
		t.Errorf("no cluster: %q", msg)
	}
}

// fleet_up's cluster names a connection by id, name or context and runs the
// instance on Kubernetes there; the reply says where it went.
func TestMCPUpOnACluster(t *testing.T) {
	f := newKubeFixture(t)
	stubDaemonIdentity(t)
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster()
	if err := state.Update(func(st *state.State) error {
		st.GetOrCreateFleet("alpha", "git@x:a.git")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	orig := jobRunCreate
	jobRunCreate = func(context.Context, string, string, string, string, bool, fleet.BackendType) error { return nil }
	t.Cleanup(func() { jobRunCreate = orig })
	cs := mcpConnect(t, f.svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "k1", "cluster": "dev"}, &out)
	if out.Target == nil || out.Target.Backend != string(fleet.BackendKubernetes) || out.Target.Cluster != dev.GetId() || out.Target.Defaulted {
		t.Fatalf("target %+v", out.Target)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.Fleets["alpha"].GetInstance("k1")
	if err != nil || inst.KubeCluster != dev.GetId() || inst.Backend != fleet.BackendKubernetes || inst.KubeKey == "" {
		t.Fatalf("record %+v, %v", inst, err)
	}
	if msg := callErr(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "k2", "cluster": "dev", "backend": "coder"}); !strings.Contains(msg, "not coder") {
		t.Errorf("cluster with another backend: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "k3", "cluster": "nope"}); !strings.Contains(msg, "nope") {
		t.Errorf("an unknown cluster: %q", msg)
	}
}

// The agent tools take a cluster (id, name or context), stored as the
// connection id; kubernetes with no cluster is the only connection, and no
// backend at all stays devcontainer (agents fire unattended, never on the
// remembered default). An update keeps the agent's connection unless told.
func TestMCPAgentOnACluster(t *testing.T) {
	f := newKubeFixture(t)
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster()
	if err := state.Update(func(st *state.State) error {
		st.GetOrCreateFleet("alpha", "git@x:a.git")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendKubernetes), DefaultKubeCluster: dev.GetId()}); err != nil {
		t.Fatal(err)
	}
	cs := mcpConnect(t, f.svc)
	agent := func(out AutomationOutput, name string) mcpAgent {
		t.Helper()
		for _, a := range out.Agents {
			if a.Name == name {
				return a
			}
		}
		t.Fatalf("no agent %s in %+v", name, out.Agents)
		return mcpAgent{}
	}

	var out AutomationOutput
	// A fresh output per call: decoding into the last one would keep the
	// fields a reply omits (an empty cluster).
	call := func(tool string, args map[string]any) {
		t.Helper()
		out = AutomationOutput{}
		callJSON(t, cs, tool, args, &out)
	}
	call("fleet_agent_create", map[string]any{"fleet": "alpha", "name": "named", "cluster": "dev"})
	if a := agent(out, "named"); a.Backend != string(fleet.BackendKubernetes) || a.Cluster != dev.GetId() {
		t.Fatalf("named agent %+v, want kubernetes on %s", a, dev.GetId())
	}
	call("fleet_agent_create", map[string]any{"fleet": "alpha", "name": "only", "backend": "kubernetes"})
	if a := agent(out, "only"); a.Cluster != dev.GetId() {
		t.Fatalf("kubernetes without a cluster = %+v, want the only connection", a)
	}
	call("fleet_agent_create", map[string]any{"fleet": "alpha", "name": "plain"})
	if a := agent(out, "plain"); a.Backend != string(fleet.BackendDevcontainer) || a.Cluster != "" {
		t.Fatalf("an agent with no backend = %+v, want devcontainer (not the remembered cluster)", a)
	}

	call("fleet_agent_update", map[string]any{"fleet": "alpha", "name": "named", "command": "claude '${PROMPT}'"})
	if a := agent(out, "named"); a.Cluster != dev.GetId() {
		t.Fatalf("an update of the command moved the agent: %+v", a)
	}
	call("fleet_agent_update", map[string]any{"fleet": "alpha", "name": "named", "backend": "coder"})
	if a := agent(out, "named"); a.Backend != string(fleet.BackendCoder) || a.Cluster != "" {
		t.Fatalf("a move to coder = %+v, want coder with no cluster", a)
	}
	call("fleet_agent_update", map[string]any{"fleet": "alpha", "name": "named", "cluster": "minikube"})
	if a := agent(out, "named"); a.Backend != string(fleet.BackendKubernetes) || a.Cluster != dev.GetId() {
		t.Fatalf("a move by context = %+v, want kubernetes on %s", a, dev.GetId())
	}

	if msg := callErr(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "x", "cluster": "dev", "backend": "coder"}); !strings.Contains(msg, "not coder") {
		t.Errorf("cluster with another backend: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "y", "cluster": "nope"}); !strings.Contains(msg, "nope") {
		t.Errorf("an unknown cluster: %q", msg)
	}
}

// TestClusterTestNamesStorageClasses: the classes TestKubeCluster (and
// fleet_cluster_test) names reach the preflight as the ones to test now,
// with the connection's storage test cache; an unsaved connection's draft
// gets none.
func TestClusterTestNamesStorageClasses(t *testing.T) {
	f := newKubeFixture(t)
	for _, sc := range []struct{ name, provisioner string }{{"filestore", "filestore.csi.storage.gke.io"}, {"nfs", "nfs.csi.k8s.io"}, {"cephfs", "cephfs.csi.ceph.com"}} {
		if _, err := f.apis["minikube"].StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: sc.name}, Provisioner: sc.provisioner}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f.start()
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster()
	var mu sync.Mutex
	var asked []preflight.Target
	old := fullPreflight
	fullPreflight = func(ctx context.Context, t preflight.Target, report func(*fleetgrpc.KubeCheck)) preflight.Result {
		mu.Lock()
		asked = append(asked, t)
		mu.Unlock()
		// The classes are real ones of the fake cluster, whose test pods
		// never run: what the RPC asks is recorded, not tested.
		t.TestStorage, t.ForceStorage = nil, false
		return old(ctx, t, report)
	}
	defer func() { fullPreflight = old }()
	// The connection's worker runs its own preflights meanwhile: find the
	// one asked for these classes.
	find := func(classes ...string) (preflight.Target, bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range asked {
			if slices.Equal(a.TestStorage, classes) {
				return a, true
			}
		}
		return preflight.Target{}, false
	}

	c := &streamCollector[fleetgrpc.TestKubeClusterEvent]{ctx: context.Background()}
	if err := f.svc.TestKubeCluster(&fleetgrpc.TestKubeClusterRequest{Id: dev.GetId(), StorageClasses: []string{"filestore"}}, c); err != nil {
		t.Fatal(err)
	}
	if got, ok := find("filestore"); !ok || got.StorageCache == nil || got.Deep {
		t.Fatalf("the RPC's preflight: asked %v, cache %v deep %v", ok, got.StorageCache != nil, got.Deep)
	}

	cs := mcpConnect(t, f.svc)
	var out FleetClusterTestOutput
	callJSON(t, cs, "fleet_cluster_test", map[string]any{"cluster": "dev", "storage_classes": []string{"nfs", "cephfs"}, "test_storage": true}, &out)
	if got, ok := find("nfs", "cephfs"); !ok || !got.ForceStorage {
		t.Fatalf("fleet_cluster_test did not ask the preflight for its classes (asked %v, again %v)", ok, got.ForceStorage)
	}

	// A class the cluster does not list is refused, naming the ones it does.
	nope := &streamCollector[fleetgrpc.TestKubeClusterEvent]{ctx: context.Background()}
	err := f.svc.TestKubeCluster(&fleetgrpc.TestKubeClusterRequest{Id: dev.GetId(), StorageClasses: []string{"nope"}}, nope)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), `no StorageClass "nope"`) || !strings.Contains(err.Error(), "cephfs") {
		t.Fatalf("an unlisted class: %v", err)
	}

	draft := &streamCollector[fleetgrpc.TestKubeClusterEvent]{ctx: context.Background()}
	if err := f.svc.TestKubeCluster(&fleetgrpc.TestKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}}, draft); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	drafts := 0
	for _, a := range asked {
		if a.Cluster.ID == "" {
			drafts++
			if a.StorageCache != nil {
				t.Fatal("a draft has a storage test cache")
			}
		}
	}
	if drafts != 1 {
		t.Fatalf("%d draft preflights, want 1", drafts)
	}
}
