package fleet

import (
	"strconv"
	"strings"
)

// kubeContainerIDPrefix starts the ContainerID of a Kubernetes instance.
const kubeContainerIDPrefix = "k8s:"

// shortContainerIDLen is how much of any other ContainerID display code shows:
// docker's short-ID length.
const shortContainerIDLen = 12

// KubeContainerID is the ContainerID of a Kubernetes instance, written
// "k8s:<cluster>/<namespace>/<statefulset>@g<generation>". Cluster is the
// cluster record's id, Generation counts rebuilds from 1, so the ID is unique
// across clusters, stable across stop/start and new on every rebuild.
type KubeContainerID struct {
	Cluster     string
	Namespace   string
	StatefulSet string
	Generation  int
}

// String formats the ID as stored in Instance.ContainerID.
func (id KubeContainerID) String() string {
	return kubeContainerIDPrefix + id.Cluster + "/" + id.Namespace + "/" + id.StatefulSet +
		"@g" + strconv.Itoa(id.Generation)
}

// ParseKubeContainerID parses a Kubernetes ContainerID. ok is false for every
// other backend's ID (a docker hash, a Coder or Codespaces workspace name) and
// for a malformed one. The namespace and StatefulSet are the last two
// "/"-separated fields, so a cluster id may itself contain "/". Only the
// canonical form parses: String of the result gives back containerID.
func ParseKubeContainerID(containerID string) (id KubeContainerID, ok bool) {
	rest, ok := strings.CutPrefix(containerID, kubeContainerIDPrefix)
	if !ok {
		return KubeContainerID{}, false
	}
	at := strings.LastIndex(rest, "@g")
	if at < 0 {
		return KubeContainerID{}, false
	}
	rest, genText := rest[:at], rest[at+len("@g"):]
	gen, err := strconv.Atoi(genText)
	if err != nil || gen < 1 || strconv.Itoa(gen) != genText {
		return KubeContainerID{}, false
	}
	stsSlash := strings.LastIndex(rest, "/")
	if stsSlash < 0 {
		return KubeContainerID{}, false
	}
	nsSlash := strings.LastIndex(rest[:stsSlash], "/")
	if nsSlash < 0 {
		return KubeContainerID{}, false
	}
	id = KubeContainerID{
		Cluster:     rest[:nsSlash],
		Namespace:   rest[nsSlash+1 : stsSlash],
		StatefulSet: rest[stsSlash+1:],
		Generation:  gen,
	}
	if id.Cluster == "" || id.Namespace == "" || id.StatefulSet == "" ||
		strings.Contains(id.Namespace, "@") || strings.Contains(id.StatefulSet, "@") {
		return KubeContainerID{}, false
	}
	return id, true
}

// ShortContainerID renders an instance's ContainerID for display (TUI
// messages, fleet ls, fleet up/clone): "<namespace>/<statefulset>" for a
// Kubernetes instance, otherwise the first 12 characters, docker's short ID.
func ShortContainerID(containerID string) string {
	if id, ok := ParseKubeContainerID(containerID); ok {
		return id.Namespace + "/" + id.StatefulSet
	}
	return containerID[:min(shortContainerIDLen, len(containerID))]
}
