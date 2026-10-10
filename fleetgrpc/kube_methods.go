package fleetgrpc

import (
	"fmt"
	"strings"
)

// kube_methods.go holds the pure helpers the clients of the Kubernetes
// connection RPCs share (`fleet cluster`, the MCP cluster tools), so every
// surface names a state and finds a connection the same way. Pure, like
// domain_methods.go: client packages import fleetgrpc.

// Display is the lower-case form of a connection's state ("ready",
// "owner conflict"); "unknown" for the unspecified zero value.
func (s KubeClusterState) Display() string {
	name, ok := KubeClusterState_name[int32(s)]
	if !ok || s == KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED {
		return "unknown"
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "KUBE_CLUSTER_STATE_")), "_", " ")
}

// Usable reports a state instances may be created in: READY or LIMITED. It is
// also `fleet cluster test`'s exit-0 rule.
func (s KubeClusterState) Usable() bool {
	return s == KubeClusterState_KUBE_CLUSTER_STATE_READY || s == KubeClusterState_KUBE_CLUSTER_STATE_LIMITED
}

// Display is the lower-case form of a check result ("pass", "warn");
// "unknown" for the unspecified zero value.
func (r KubeCheckResult) Display() string {
	name, ok := KubeCheckResult_name[int32(r)]
	if !ok || r == KubeCheckResult_KUBE_CHECK_RESULT_UNSPECIFIED {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "KUBE_CHECK_RESULT_"))
}

// DisplayNameOrID returns the connection's display name, else its id (what
// the daemon shows for a connection added without a name).
func (c *KubeCluster) DisplayNameOrID() string {
	if c.GetDisplayName() == "" {
		return c.GetId()
	}
	return c.GetDisplayName()
}

// FindKubeCluster finds the connection ref names: its id, else its display
// name (in any case: the daemon keeps display names unique regardless of
// case), else the context it uses when exactly one connection uses it.
func FindKubeCluster(clusters []*KubeClusterStatus, ref string) (*KubeClusterStatus, error) {
	if ref == "" {
		return nil, fmt.Errorf("no cluster named")
	}
	for _, st := range clusters {
		if st.GetCluster().GetId() == ref {
			return st, nil
		}
	}
	for _, st := range clusters {
		if name := st.GetCluster().GetDisplayName(); name != "" && strings.EqualFold(name, ref) {
			return st, nil
		}
	}
	var byContext []*KubeClusterStatus
	for _, st := range clusters {
		if st.GetCluster().GetContext() == ref {
			byContext = append(byContext, st)
		}
	}
	switch len(byContext) {
	case 0:
		return nil, fmt.Errorf("no Kubernetes connection %q", ref)
	case 1:
		return byContext[0], nil
	}
	names := make([]string, len(byContext))
	for i, st := range byContext {
		names[i] = st.GetCluster().DisplayNameOrID()
	}
	return nil, fmt.Errorf("%d connections use context %q (%s): name one", len(byContext), ref, strings.Join(names, ", "))
}
