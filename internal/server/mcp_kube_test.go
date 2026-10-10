//go:build !nok8s

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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

	callJSON(t, cs, "fleet_cluster_test", map[string]any{"cluster": "prod"}, &out)
	if out.Cluster.State != "limited" || !out.Cluster.Usable {
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
