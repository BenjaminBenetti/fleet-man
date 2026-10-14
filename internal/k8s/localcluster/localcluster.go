// Package localcluster tells a cluster running on the daemon host's own
// docker (minikube, kind, k3d, Docker Desktop) from any other, by what its
// nodes say about themselves. Image delivery loads built images straight into
// such a cluster's nodes, and the privilege policy treats it as local.
//
// It is pure Go with no client-go dependency: the caller lists the nodes and
// hands over the few fields Detect reads. Only node evidence counts, never the
// kubeconfig context's name, which anyone can choose: a context called
// "kind-dev" that points at a remote cluster must not get local treatment.
package localcluster

import (
	"regexp"
	"strings"
)

// Kind is the local cluster tool that made the cluster.
type Kind string

const (
	Minikube Kind = "minikube"
	KinD     Kind = "kind"
	K3d      Kind = "k3d"
	// DockerDesktop is Docker Desktop's built-in single-node cluster, whose
	// kubelet runs on the desktop's own docker engine.
	DockerDesktop Kind = "docker-desktop"
)

// Cluster is a detected local cluster; the zero value means none.
type Cluster struct {
	Kind Kind
	// Name is the tool's name for the cluster: the minikube profile, the kind
	// or k3d cluster name, "docker-desktop".
	Name string
}

// Local reports whether a local cluster was detected.
func (c Cluster) Local() bool { return c.Kind != "" }

// String is "<kind>:<name>" ("minikube:fleet-k8s"), or "" when not local.
func (c Cluster) String() string {
	if !c.Local() {
		return ""
	}
	return string(c.Kind) + ":" + c.Name
}

// Node is what Detect reads of one Node object.
type Node struct {
	Name       string
	Labels     map[string]string
	ProviderID string
}

// minikubeNameLabel is the label minikube puts on every node of a profile.
const minikubeNameLabel = "minikube.k8s.io/name"

// k3dNodeName is the name k3d gives a cluster's server and agent nodes.
var k3dNodeName = regexp.MustCompile(`^k3d-(.+)-(?:server|agent)-[0-9]+$`)

// Detect returns the local cluster the nodes belong to, or the zero Cluster.
// Every node must agree: a cluster that mixes, say, a minikube node with any
// other is not treated as local. No nodes (none readable) is not local.
//
//   - minikube labels each node minikube.k8s.io/name=<profile>;
//   - kind sets each node's providerID to kind://<provider>/<cluster>/<node>;
//   - k3d names nodes k3d-<cluster>-server-<n> and k3d-<cluster>-agent-<n>;
//   - Docker Desktop's single node is named docker-desktop.
func Detect(nodes []Node) Cluster {
	if len(nodes) == 0 {
		return Cluster{}
	}
	var found Cluster
	for i, n := range nodes {
		c := detectNode(n)
		if !c.Local() || (i > 0 && c != found) {
			return Cluster{}
		}
		found = c
	}
	return found
}

func detectNode(n Node) Cluster {
	if name := n.Labels[minikubeNameLabel]; name != "" {
		return Cluster{Kind: Minikube, Name: name}
	}
	if rest, ok := strings.CutPrefix(n.ProviderID, "kind://"); ok {
		// <provider>/<cluster>/<node>
		parts := strings.Split(rest, "/")
		if len(parts) == 3 && parts[1] != "" {
			return Cluster{Kind: KinD, Name: parts[1]}
		}
		return Cluster{}
	}
	if m := k3dNodeName.FindStringSubmatch(n.Name); m != nil {
		return Cluster{Kind: K3d, Name: m[1]}
	}
	if n.Name == "docker-desktop" {
		return Cluster{Kind: DockerDesktop, Name: "docker-desktop"}
	}
	return Cluster{}
}
