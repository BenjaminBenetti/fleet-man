//go:build !nok8s

package kubernetes

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

func replicas(n int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Replicas: &n}}
}

func podIn(phase corev1.PodPhase, mutate ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: phase},
	}
	for _, m := range mutate {
		m(pod)
	}
	return pod
}

func devRunning(ready bool) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: DevContainer, Ready: ready, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	}
}

func devWaiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: DevContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
	}
}

func readyCondition(status corev1.ConditionStatus) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: status})
	}
}

func TestStatusTable(t *testing.T) {
	now := metav1.Now()
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name   string
		in     statusInput
		live   backend.LiveStatus
		reason string
		agent  AgentState
	}{
		{"not watched yet", statusInput{health: errNotWatched}, backend.LiveStatusUnknown, errNotWatched.Error(), AgentNone},
		{"not listed yet", statusInput{health: errNotSynced, setGone: true}, backend.LiveStatusUnknown, errNotSynced.Error(), AgentNone},
		{"unreachable is never missing", statusInput{health: errors.New("dial tcp: connection refused"), setGone: true, supervised: true}, backend.LiveStatusUnknown, "cluster unreachable", AgentUnknown},
		{"login expired", statusInput{health: apierrors.NewUnauthorized("expired")}, backend.LiveStatusUnknown, "cluster login expired or refused", AgentNone},
		{"forbidden", statusInput{health: apierrors.NewForbidden(gr, "", errors.New("no"))}, backend.LiveStatusUnknown, "the connection may not watch its instances", AgentNone},
		{"StatefulSet gone", statusInput{setGone: true}, backend.LiveStatusMissing, "", AgentNone},
		{"neither object", statusInput{}, backend.LiveStatusMissing, "", AgentNone},
		{"tampered", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(true)), tampered: &TamperedError{}}, backend.LiveStatusUnknown, CodeTampered, AgentNone},
		{"scaled to 0", statusInput{set: replicas(0), supervised: true}, backend.LiveStatusStopped, "", AgentNone},
		{"scaled to 0, pod still going", statusInput{set: replicas(0), pod: podIn(corev1.PodRunning, devRunning(true), func(p *corev1.Pod) { p.DeletionTimestamp = &now })}, backend.LiveStatusStopped, "", AgentNone},
		{"crash loop, OOM", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devWaiting("CrashLoopBackOff", "")), loop: &CrashLoop{OOM: true}}, backend.LiveStatusUnknown, "crash loop: OOMKilled", AgentNone},
		{"crash loop", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning), loop: &CrashLoop{Reason: "Error"}}, backend.LiveStatusUnknown, "crash loop: Error", AgentNone},
		{"no pod yet", statusInput{set: replicas(1)}, backend.LiveStatusUnknown, "no pod yet", AgentNone},
		{"terminating", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(true), func(p *corev1.Pod) { p.DeletionTimestamp = &now })}, backend.LiveStatusUnknown, "Terminating", AgentNone},
		{"creating", statusInput{set: replicas(1), pod: podIn(corev1.PodPending, devWaiting("ContainerCreating", ""))}, backend.LiveStatusUnknown, "ContainerCreating", AgentNone},
		{"pull back-off", statusInput{set: replicas(1), pod: podIn(corev1.PodPending, devWaiting("ImagePullBackOff", "Back-off pulling image \"x\"\nmore"))}, backend.LiveStatusUnknown, `ImagePullBackOff: Back-off pulling image "x"`, AgentNone},
		{"unschedulable", statusInput{set: replicas(1), pod: podIn(corev1.PodPending, func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available"}}
		})}, backend.LiveStatusUnknown, "Unschedulable: 0/1 nodes are available", AgentNone},
		{"pending", statusInput{set: replicas(1), pod: podIn(corev1.PodPending)}, backend.LiveStatusUnknown, "Pending", AgentNone},
		{"evicted", statusInput{set: replicas(1), pod: podIn(corev1.PodFailed, func(p *corev1.Pod) {
			p.Status.Reason, p.Status.Message = "Evicted", "The node was low on resource: memory."
		})}, backend.LiveStatusUnknown, "Evicted: The node was low on resource: memory.", AgentNone},
		{"running", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(true), readyCondition(corev1.ConditionTrue))}, backend.LiveStatusRunning, "", AgentNone},
		{"running, no conditions", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(false))}, backend.LiveStatusRunning, "", AgentNone},
		{"running, not ready", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(false), readyCondition(corev1.ConditionFalse))}, backend.LiveStatusUnknown, "not ready", AgentNone},
		{"running, agent connected", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(true)), supervised: true, connected: true}, backend.LiveStatusRunning, "", AgentConnected},
		{"running, agent reconnecting", statusInput{set: replicas(1), pod: podIn(corev1.PodRunning, devRunning(true)), supervised: true}, backend.LiveStatusRunning, "", AgentReconnecting},
		{"starting, no agent yet", statusInput{set: replicas(1), pod: podIn(corev1.PodPending), supervised: true}, backend.LiveStatusUnknown, "Pending", AgentNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := observe(tc.in)
			if o.Live != tc.live || o.Reason != tc.reason || o.Agent != tc.agent {
				t.Fatalf("observe = %s %q agent %d, want %s %q agent %d", o.Live, o.Reason, o.Agent, tc.live, tc.reason, tc.agent)
			}
			if tc.in.pod != nil && o.Node != "node-a" {
				t.Fatalf("node %q", o.Node)
			}
		})
	}
}
