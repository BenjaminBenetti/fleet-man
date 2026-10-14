//go:build !nok8s

package kubernetes

import (
	"errors"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// status.go is the instance status table: what fleetd tells of an instance
// from its connection's informers, with no API call.
//
//	Observation                                  | Live
//	---------------------------------------------+---------------------------
//	not watched yet, informer unsynced,          | unknown (+ why), never
//	API unreachable, 401/403                     | missing
//	StatefulSet gone (a synced, answering cache)  | missing
//	pod refused by verification                  | unknown + kubernetes:tampered
//	replicas 0 during a move off a node          | unknown (not a stop: the
//	(AnnotationMovingOff)                        | record stays running)
//	replicas 0                                   | stopped
//	crash loop not parked yet                    | unknown + the loop
//	no pod, Pending, ContainerCreating,          | unknown + reason
//	Terminating, Failed                          |
//	pod running and ready                        | running (+ agent state)

// AgentState is the in-pod agent session as the pool sees it.
type AgentState int

const (
	// AgentNone: the pool does not supervise the instance (or it does not
	// run).
	AgentNone AgentState = iota
	// AgentConnected: a session is up.
	AgentConnected
	// AgentReconnecting: the pod runs, the pool keeps an agent up in it, and
	// none is connected right now (a bootstrap is in flight or backing off).
	AgentReconnecting
	// AgentUnknown: the API server does not answer, so neither does the
	// pod.
	AgentUnknown
)

// Observation is the status table's answer for one instance.
type Observation struct {
	Live backend.LiveStatus
	// Reason says why the instance is not running, or why its state is not
	// known ("" when it runs, or is stopped as asked).
	Reason string
	// Node is where the pod runs ("" before it is scheduled).
	Node  string
	Agent AgentState
	// Restart is the latest restart the cluster made on its own (nil:
	// none), and SessionsEnded whether it killed every process (an
	// out-of-memory kill).
	Restart       *backend.Restart
	SessionsEnded bool
	// CrashLoop is set while the instance is crash looping and not parked.
	CrashLoop *CrashLoop
	// Tampered is the verification failure of the pod, when it was refused.
	Tampered *TamperedError
	// Holder: the pod holds its fleet's share in single-node mode (it is
	// required beside the share group's running pods), so its fleet moves
	// off a node only as a whole (evacuate.go).
	Holder bool
	// Disrupted says why the cluster is emptying the node a holder's pod is
	// on ("cordoned"; NodeDisruption), "" when it is not (or the node is not
	// watched).
	Disrupted string
	// MovingOff is the node the instance's fleet is being moved off (its
	// StatefulSet's AnnotationMovingOff): set from before the move stops it
	// until it runs again.
	MovingOff string
}

// statusInput is what the table is computed from.
type statusInput struct {
	// health is why the informers' view is not current (not watched yet,
	// not synced, not answering); nil when it is.
	health     error
	set        *appsv1.StatefulSet
	setGone    bool
	pod        *corev1.Pod
	tampered   *TamperedError
	loop       *CrashLoop
	supervised bool
	connected  bool
}

// errNotWatched: no informer runs for the instance's connection yet.
var errNotWatched = errors.New("the cluster's instances are not watched yet")

// observe is the status table.
func observe(in statusInput) Observation {
	var o Observation
	if in.pod != nil {
		o.Node = in.pod.Spec.NodeName
	}
	o.Agent = agentState(in)
	switch {
	case in.health != nil:
		o.Live, o.Reason = backend.LiveStatusUnknown, healthReason(in.health)
	case in.setGone || (in.set == nil && in.pod == nil):
		o.Live = backend.LiveStatusMissing
	case in.tampered != nil:
		o.Live, o.Reason = backend.LiveStatusUnknown, CodeTampered
	case in.set != nil && in.set.Spec.Replicas != nil && *in.set.Spec.Replicas == 0 && in.set.Annotations[names.AnnotationMovingOff] != "":
		// Stopped by a move (evacuate.go), which starts it again: not a stop
		// a conclusive probe may record (the live-status pass would record
		// the instance stopped, and the move then leave it so).
		o.Live = backend.LiveStatusUnknown
	case in.set != nil && in.set.Spec.Replicas != nil && *in.set.Spec.Replicas == 0:
		o.Live = backend.LiveStatusStopped
	case in.loop != nil:
		o.Live, o.Reason = backend.LiveStatusUnknown, loopReason(in.loop)
	case in.pod == nil:
		o.Live, o.Reason = backend.LiveStatusUnknown, "no pod yet"
	default:
		o.Live, o.Reason = podLive(in.pod)
	}
	if o.Live != backend.LiveStatusRunning && o.Agent == AgentReconnecting {
		o.Agent = AgentNone
	}
	return o
}

func agentState(in statusInput) AgentState {
	switch {
	case !in.supervised:
		return AgentNone
	case in.connected:
		return AgentConnected
	case in.health != nil:
		return AgentUnknown
	}
	return AgentReconnecting
}

// healthReason words why the informers' view is not current.
func healthReason(err error) string {
	switch {
	case errors.Is(err, errNotWatched), errors.Is(err, errNotSynced):
		return err.Error()
	case apierrors.IsUnauthorized(err):
		return "cluster login expired or refused"
	case apierrors.IsForbidden(err):
		return "the connection may not watch its instances"
	}
	return "cluster unreachable"
}

func loopReason(l *CrashLoop) string {
	if l.OOM {
		return "crash loop: OOMKilled"
	}
	if l.Reason != "" {
		return "crash loop: " + l.Reason
	}
	return "CrashLoopBackOff"
}

// podLive is the pod's own part of the table.
func podLive(pod *corev1.Pod) (backend.LiveStatus, string) {
	if pod.DeletionTimestamp != nil {
		return backend.LiveStatusUnknown, "Terminating"
	}
	dev := containerStatus(pod, DevContainer)
	switch pod.Status.Phase {
	case corev1.PodRunning:
		if dev != nil && dev.State.Running != nil && (dev.Ready || !readyConditionFalse(pod)) {
			return backend.LiveStatusRunning, ""
		}
		if r := waitingReason(dev); r != "" {
			return backend.LiveStatusUnknown, r
		}
		return backend.LiveStatusUnknown, "not ready"
	case corev1.PodPending:
		if s := initContainerStatus(pod, podspec.ShareInit); s != nil {
			if t := s.State.Terminated; t != nil && t.ExitCode != 0 {
				return backend.LiveStatusUnknown, podspec.ShareInit + ": " + joinReason("", firstNonEmpty(t.Message, t.Reason))
			}
			if s.State.Waiting != nil && s.State.Waiting.Reason != "PodInitializing" {
				return backend.LiveStatusUnknown, podspec.ShareInit + ": " + waitingReason(s)
			}
		}
		// The dev container waits on the docker engine's sidecar
		// (PodInitializing) while that one cannot start.
		if s := initContainerStatus(pod, podspec.DockerSidecar); s != nil && s.State.Waiting != nil && s.State.Waiting.Reason != "PodInitializing" {
			return backend.LiveStatusUnknown, podspec.DockerSidecar + ": " + waitingReason(s)
		}
		if r := waitingReason(dev); r != "" {
			return backend.LiveStatusUnknown, r
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				return backend.LiveStatusUnknown, joinReason(c.Reason, c.Message)
			}
		}
		return backend.LiveStatusUnknown, "Pending"
	case corev1.PodFailed, corev1.PodSucceeded:
		if pod.Status.Reason != "" {
			return backend.LiveStatusUnknown, joinReason(pod.Status.Reason, pod.Status.Message)
		}
		return backend.LiveStatusUnknown, string(pod.Status.Phase)
	}
	return backend.LiveStatusUnknown, "Unknown"
}

// readyConditionFalse reports a Ready condition that is not True (a pod
// without one, as a fake or a just-started pod may be, is not held back).
func readyConditionFalse(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status != corev1.ConditionTrue
		}
	}
	return false
}

func waitingReason(s *corev1.ContainerStatus) string {
	if s == nil || s.State.Waiting == nil {
		return ""
	}
	return joinReason(s.State.Waiting.Reason, s.State.Waiting.Message)
}

// joinReason is "Reason: message", the message cut to one short line.
func joinReason(reason, msg string) string {
	msg = strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
	if len(msg) > 160 {
		msg = msg[:157] + "..."
	}
	switch {
	case reason == "":
		return msg
	case msg == "":
		return reason
	}
	return reason + ": " + msg
}

// Observe is the instance's row of the status table, from its connection's
// informers (no API call). Until the informers run (Track starts them) and
// have listed, the instance is unknown, never missing.
func (p *Pool) Observe(clusterID, sts string) Observation {
	k := trackKey{clusterID, sts}
	in := statusInput{health: errNotWatched}
	w := p.runningWatcher(clusterID)
	if w != nil {
		in.health = w.health()
	}
	p.instMu.Lock()
	t := p.tracks[k]
	if t != nil {
		in.set, in.setGone, in.pod, in.tampered, in.loop = t.set, t.setGone, t.pod, t.tampered, t.loop
	}
	_, in.supervised = p.supervised[k]
	var restart *backend.Restart
	ended := false
	if t != nil && t.last != nil {
		r := *t.last
		restart, ended = &r, t.ended
	}
	p.instMu.Unlock()
	if w != nil && in.health == nil && t == nil {
		// Listed, and the cluster has neither its StatefulSet nor its pod.
		in.setGone = true
	}
	for _, s := range p.slotsOf(k) {
		if s.live() != nil {
			in.connected = true
		}
	}
	o := observe(in)
	o.Restart, o.SessionsEnded = restart, ended
	o.CrashLoop = in.loop
	if in.set != nil && in.set.Spec.Replicas != nil && *in.set.Spec.Replicas == 0 {
		o.CrashLoop = nil
	}
	o.Tampered = in.tampered
	if in.set != nil {
		o.MovingOff = in.set.Annotations[names.AnnotationMovingOff]
	}
	if in.pod != nil && in.pod.DeletionTimestamp == nil && singleNodeHolder(in.pod) {
		o.Holder = true
		if o.Node != "" {
			o.Disrupted = p.NodeDisruptionOf(clusterID, o.Node)
		}
	}
	return o
}

// firstNonEmpty is the first of vs that is not empty.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
