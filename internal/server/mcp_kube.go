package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// mcp_kube.go is the MCP view of the daemon's Kubernetes cluster
// connections: fleet_cluster_list and fleet_cluster_test, over the same
// ListKubeClusters / TestKubeCluster handlers `fleet cluster ls` and `fleet
// cluster test` call, so the three surfaces cannot disagree. A cluster is
// named by its id, its display name or (when one connection uses it) its
// kubeconfig context, as on the CLI (fleetgrpc.FindKubeCluster).

// mcpKubeCluster is one connection with its health.
type mcpKubeCluster struct {
	ID              string         `json:"id" jsonschema:"the connection's id: what an instance on it records as its cluster"`
	Name            string         `json:"name" jsonschema:"the display name, else the id"`
	Context         string         `json:"context" jsonschema:"the kubeconfig context, in kubeconfig files on the daemon host"`
	Namespace       string         `json:"namespace"`
	KubeconfigPaths []string       `json:"kubeconfig_paths,omitempty" jsonschema:"kubeconfig files on the daemon host"`
	Server          string         `json:"server,omitempty" jsonschema:"the API server URL, when last checked"`
	State           string         `json:"state" jsonschema:"health: checking, ready, limited, unreachable, unauthorized, forbidden, context missing, plugin missing, misconfigured or owner conflict"`
	Usable          bool           `json:"usable" jsonschema:"true when the state is ready or limited: instances can run there"`
	Message         string         `json:"message,omitempty" jsonschema:"why the state is not ready"`
	Warnings        []string       `json:"warnings,omitempty" jsonschema:"exposure warnings that never block (e.g. privileged pods allowed)"`
	CheckedAt       string         `json:"checked_at,omitempty"`
	DeepCheckedAt   string         `json:"deep_checked_at,omitempty" jsonschema:"when the last deep test (fleet_cluster_test deep) finished"`
	Settings        map[string]any `json:"settings" jsonschema:"every field the connection sets, by its kube.proto KubeCluster field name; an absent field takes its default"`
	Capabilities    map[string]any `json:"capabilities,omitempty" jsonschema:"what the last full check found (version, admission, storage classes, nodes, quotas, ...), by kube.proto KubeCapabilities field name"`
}

// mcpKubeCheck is one preflight check.
type mcpKubeCheck struct {
	Name   string `json:"name"`
	Result string `json:"result" jsonschema:"pass, warn, fail or skipped"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func toMCPKubeCluster(st *fleetgrpc.KubeClusterStatus) (mcpKubeCluster, error) {
	c := st.GetCluster()
	out := mcpKubeCluster{
		ID: c.GetId(), Name: c.DisplayNameOrID(), Context: c.GetContext(), Namespace: c.GetNamespace(),
		KubeconfigPaths: c.GetKubeconfigPaths(), Server: c.GetServer(),
		State: st.GetState().Display(), Usable: st.GetState().Usable(), Message: st.GetMessage(), Warnings: st.GetWarnings(),
	}
	if ts := st.GetCheckedAt(); ts != nil {
		out.CheckedAt = ts.AsTime().UTC().Format(time.RFC3339)
	}
	if ts := st.GetDeepCheckedAt(); ts != nil {
		out.DeepCheckedAt = ts.AsTime().UTC().Format(time.RFC3339)
	}
	var err error
	if out.Settings, err = protoObject(c); err != nil {
		return out, err
	}
	if out.Settings == nil {
		out.Settings = map[string]any{}
	}
	if caps := st.GetCapabilities(); caps != nil {
		if out.Capabilities, err = protoObject(caps); err != nil {
			return out, err
		}
	}
	return out, nil
}

// protoObject is m as a JSON object keyed by proto field names.
func protoObject(m proto.Message) (map[string]any, error) {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// --- fleet_cluster_list ---

type FleetClusterListInput struct {
	Cluster string `json:"cluster,omitempty" jsonschema:"optional connection (id, name or context) to return alone; empty lists every connection"`
}

type FleetClusterListOutput struct {
	Clusters []mcpKubeCluster `json:"clusters"`
}

func (s *service) mcpClusterList(ctx context.Context, _ *mcp.CallToolRequest, in FleetClusterListInput) (*mcp.CallToolResult, FleetClusterListOutput, error) {
	reply, err := s.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		return nil, FleetClusterListOutput{}, mcpErr(err)
	}
	clusters := reply.GetClusters()
	if in.Cluster != "" {
		st, err := fleetgrpc.FindKubeCluster(clusters, in.Cluster)
		if err != nil {
			return nil, FleetClusterListOutput{}, err
		}
		clusters = []*fleetgrpc.KubeClusterStatus{st}
	}
	out := FleetClusterListOutput{Clusters: []mcpKubeCluster{}}
	for _, st := range clusters {
		c, err := toMCPKubeCluster(st)
		if err != nil {
			return nil, FleetClusterListOutput{}, err
		}
		out.Clusters = append(out.Clusters, c)
	}
	return nil, out, nil
}

// --- fleet_cluster_test ---

type FleetClusterTestInput struct {
	Cluster string `json:"cluster" jsonschema:"the connection to test: its id, name or context"`
	Deep    bool   `json:"deep,omitempty" jsonschema:"also run the throwaway-pod probes (a pod pulls through the gateway; pods test NetworkPolicy enforcement and reach to 169.254.169.254); takes minutes"`
	// StorageClasses run the multi-node storage test now.
	StorageClasses []string `json:"storage_classes,omitempty" jsonschema:"StorageClasses to run the multi-node storage test on now (an RWX volume mounted by pods on two nodes; about a minute each), cached result or not; naming a class whose volumes have a large minimum size (Filestore, 1Ti) is the consent Auto asks for before it tests one"`
}

type FleetClusterTestOutput struct {
	Cluster mcpKubeCluster `json:"cluster" jsonschema:"the connection with the health this test found"`
	Checks  []mcpKubeCheck `json:"checks" jsonschema:"every check, in the order run"`
}

func (s *service) mcpClusterTest(ctx context.Context, _ *mcp.CallToolRequest, in FleetClusterTestInput) (*mcp.CallToolResult, FleetClusterTestOutput, error) {
	if in.Cluster == "" {
		return nil, FleetClusterTestOutput{}, errors.New("cluster is required")
	}
	reply, err := s.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		return nil, FleetClusterTestOutput{}, mcpErr(err)
	}
	st, err := fleetgrpc.FindKubeCluster(reply.GetClusters(), in.Cluster)
	if err != nil {
		return nil, FleetClusterTestOutput{}, err
	}
	c := &streamCollector[fleetgrpc.TestKubeClusterEvent]{ctx: ctx}
	if err := s.TestKubeCluster(&fleetgrpc.TestKubeClusterRequest{Id: st.GetCluster().GetId(), Deep: in.Deep, StorageClasses: in.StorageClasses}, c); err != nil {
		return nil, FleetClusterTestOutput{}, mcpErr(err)
	}
	var result *fleetgrpc.KubeClusterStatus
	for _, ev := range c.events {
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
	if result == nil {
		return nil, FleetClusterTestOutput{}, errors.New("the test ended without a result")
	}
	cluster, err := toMCPKubeCluster(result)
	if err != nil {
		return nil, FleetClusterTestOutput{}, err
	}
	out := FleetClusterTestOutput{Cluster: cluster, Checks: []mcpKubeCheck{}}
	for _, ch := range result.GetChecks() {
		out.Checks = append(out.Checks, mcpKubeCheck{Name: ch.GetName(), Result: ch.GetResult().Display(), Detail: ch.GetDetail(), Fix: ch.GetFix()})
	}
	return nil, out, nil
}
