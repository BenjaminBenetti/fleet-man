package tui

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// kube_row.go is what an instance row says of an instance on a Kubernetes
// cluster: ☸ and its connection's name beside the branch, and after its
// status the notes InstanceRuntime.kube and the connection's health give
// (the pod not ready, the agent reconnecting, the cluster unreachable or its
// login expired, a restart the cluster made), or for a failed or parked one
// what its error code says. Notes are Muted; the ✕ of a connection that
// cannot be used is Error.

// kubeRowNote is the note a running Kubernetes instance's row carries after
// its status: the restart the cluster made on its own, until the next attach
// clears it ("sessions reset: node drained"). An out-of-memory restart, which
// killed every process and not only the sessions, says so. "" when there is
// none (and for every other backend, which has no InstanceRuntime.kube).
func kubeRowNote(rt *fleetgrpc.InstanceRuntime) string {
	k := rt.GetKube()
	switch {
	case k.GetRestartCause() == "":
		return ""
	case k.GetSessionsEnded():
		return "restarted: out of memory, sessions ended"
	}
	return "sessions reset: " + k.GetRestartCause()
}

// kubeClusterNote is what a connection's health means for the instances on
// it: "cluster unreachable" when its API server does not answer, "cluster
// login expired" when it refuses the connection's credentials, "" otherwise
// (also for a connection the TUI has not heard of).
func kubeClusterNote(st *fleetgrpc.KubeClusterStatus) string {
	switch st.GetState() {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE:
		return "cluster unreachable"
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED:
		return "cluster login expired"
	}
	return ""
}

// kubePodNote words why the instance's pod is not ready (pod_reason): a
// kubelet reason as "pod: <reason>", a refused pod as tampered with, and the
// pool's own words about its view of the cluster as they are.
func kubePodNote(reason string) string {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return ""
	case kubeerr.Code(reason).Is(kubeerr.Tampered):
		return "pod tampered with: refused"
	case strings.Contains(reason, " "):
		// "cluster unreachable", "cluster login expired or refused", "no pod
		// yet", "crash loop: …": already a phrase.
		return reason
	}
	return "pod: " + reason
}

// kubeRunningNotes are the notes of a running Kubernetes instance's row, in
// the order they show: the pod, the node it is moving off, the agent, the
// cluster's health, the last restart. agentUnknown reports whether the
// agent indicator and the stats are to be drawn unknown (the captures keep
// failing, or the cluster's health says nothing answers).
func (m *model) kubeRunningNotes(inst *fleet.Instance, rt *fleetgrpc.InstanceRuntime) (notes []string, agentUnknown bool) {
	k := rt.GetKube()
	cluster := kubeClusterNote(m.kubeStatus(inst.KubeCluster))
	add := func(note string) {
		if note == "" {
			return
		}
		for _, n := range notes {
			if n == note || strings.HasPrefix(n, note) {
				return
			}
		}
		notes = append(notes, note)
	}
	add(kubePodNote(k.GetPodReason()))
	if k.GetNodeCordonedMoving() {
		add("node cordoned: fleet moving")
	}
	agentUnknown = rt.GetAgentActivity() == fleetgrpc.AgentActivity_AGENT_ACTIVITY_UNKNOWN || cluster != "" ||
		k.GetAgent() == fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_UNKNOWN
	if k.GetAgent() == fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_RECONNECTING && cluster == "" {
		add("agent reconnecting")
	}
	add(cluster)
	add(kubeRowNote(rt))
	return notes, agentUnknown
}

// kubeRowTarget is what a Kubernetes instance's row shows beside its branch:
// ☸ and its connection's name, "(missing)" once the daemon no longer has the
// connection, and a ✕ while the connection cannot be used.
func (m *model) kubeRowTarget(inst *fleet.Instance) string {
	label := deployTargetLabel(m, inst.DeployTarget())
	st := m.kubeStatus(inst.KubeCluster)
	switch {
	case st == nil && m.kubeClusters != nil:
		return dimStyle.Render(label + " (missing)")
	case st != nil && !st.GetState().Usable() && st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING:
		return dimStyle.Render(label+" ") + kubeErrStyle.Render(kubeGlyphFailed)
	}
	return dimStyle.Render(label)
}

// kubeRecoveryCode is the kubeerr code of a Kubernetes instance's error that
// the recovery dialog explains: a failed instance's, or a stopped one's that
// the cluster parked (a crash loop, out of memory). ok is false for every
// other instance.
func kubeRecoveryCode(inst *fleet.Instance) (code kubeerr.Code, detail string, ok bool) {
	if inst == nil || inst.Backend != fleet.BackendKubernetes {
		return "", "", false
	}
	switch inst.Status {
	case fleet.StatusFailed, fleet.StatusStopped:
	default:
		return "", "", false
	}
	code, detail, ok = kubeerr.Parse(inst.Error)
	if !ok {
		return "", "", false
	}
	if inst.Status == fleet.StatusStopped && !code.Is(kubeerr.OOMKilled) && !code.Is(kubeerr.CrashLoop) {
		// A stopped instance's error is a parked crash loop's; any other
		// is not this record's story any more.
		return "", "", false
	}
	return code, detail, true
}

// kubePodRecoveryCode is the kubeerr code of a running Kubernetes instance's
// pod that the recovery dialog explains: a pod fleet refused as tampered
// with (pod_reason), which no session reaches. ok is false for every other
// instance.
func kubePodRecoveryCode(inst *fleet.Instance, rt *fleetgrpc.InstanceRuntime) (kubeerr.Code, bool) {
	if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.Status != fleet.StatusRunning {
		return "", false
	}
	code := kubeerr.Code(strings.TrimSpace(rt.GetKube().GetPodReason()))
	if !code.Is(kubeerr.Tampered) {
		return "", false
	}
	return code, true
}

// kubeClassWords is each class as a row note says it.
var kubeClassWords = map[kubeerr.Code]string{
	kubeerr.Unreachable:     "cluster unreachable",
	kubeerr.Unauthorized:    "cluster login expired",
	kubeerr.Forbidden:       "forbidden by the cluster",
	kubeerr.AdmissionDenied: "admission denied",
	kubeerr.Quota:           "over quota",
	kubeerr.Unsupported:     "not supported on Kubernetes",
	kubeerr.Misconfigured:   "connection misconfigured",
	kubeerr.ImageBuild:      "image build failed",
	kubeerr.ImagePush:       "image delivery failed",
	kubeerr.ImagePull:       "image pull failed",
	kubeerr.PVCPending:      "disk did not bind",
	kubeerr.Unschedulable:   "unschedulable",
	kubeerr.CrashLoop:       "crash loop",
	kubeerr.OOMKilled:       "out of memory",
	kubeerr.Timeout:         "timed out",
	kubeerr.ClusterMissing:  "cluster missing",
	kubeerr.Interrupted:     "interrupted",
	kubeerr.RebuildFailed:   "rebuild failed",
	kubeerr.Tampered:        "pod tampered with",
	kubeerr.OwnerConflict:   "owner conflict",
	kubeerr.Unavailable:     "not available yet",
}

// kubeFailedNote is the note after a failed or parked Kubernetes row's
// status: what its error code says ("interrupted · d delete", "parked: out of
// memory"); "" for any other row. An interrupted create is not retried yet,
// so its row offers what is: destroy it and create it again.
func kubeFailedNote(inst *fleet.Instance) string {
	code, _, ok := kubeRecoveryCode(inst)
	if !ok {
		return ""
	}
	words := kubeClassWords[code.Class()]
	switch {
	case inst.Status == fleet.StatusStopped:
		return "parked: " + words
	case code.Is(kubeerr.Interrupted):
		return words + " · d delete"
	}
	return words
}
