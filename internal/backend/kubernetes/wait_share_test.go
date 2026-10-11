//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// shareInitEnded is a pod whose share-layout init container ended with
// exit and message, its dev container held back.
func shareInitEnded(exit int32, msg string) func(*corev1.Pod) {
	return shareInitFailed(exit, msg, shareLayoutAttempts-1)
}

// shareInitFailed is shareInitEnded after restarts runs before it.
func shareInitFailed(exit int32, msg string, restarts int32) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		waiting("PodInitializing", "")(p)
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: podspec.ShareInit, RestartCount: restarts, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: exit, Message: msg},
		}}}
	}
}

// TestPodWaitNamesTheShareLayoutsFailure: a share-layout init container
// that keeps failing ends the wait with its reason (it lays out the same
// tree every time); one that failed once or twice (a full share that got
// room) is waited for; one that is fine does not end it.
func TestPodWaitNamesTheShareLayoutsFailure(t *testing.T) {
	d, _ := LoadDeadlines(func(string) string { return "" })
	w := newPodWait(d, DevContainer, "the instance pod")
	msg := "fleet share layout: laying out the fleet's share: /opt/fleet/run/sync/share/roots/.claude is a file where the fleet's folder belongs; remove it from the share"
	for restarts := int32(0); restarts < shareLayoutAttempts-1; restarts++ {
		if err := w.observe(time.Now(), waitPod(shareInitFailed(1, msg, restarts)), nil); err != nil {
			t.Fatalf("a layout that failed %d time(s): %v, want waited for", restarts+1, err)
		}
	}
	err := w.observe(time.Now(), waitPod(shareInitEnded(1, msg)), nil)
	var pe *PodError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "roots/.claude is a file") {
		t.Fatalf("observe = %v, want the layout's reason", err)
	}
	if code, _ := kubeerr.Of(err); !code.Is(kubeerr.Misconfigured) {
		t.Fatalf("code %s", code)
	}
	if err := w.observe(time.Now(), waitPod(shareInitEnded(0, "")), nil); err != nil {
		t.Fatalf("a layout that succeeded: %v", err)
	}
	if live, why := podLive(waitPod(shareInitEnded(1, msg))); live != backend.LiveStatusUnknown || !strings.Contains(why, podspec.ShareInit+": ") || !strings.Contains(why, "is a file") {
		t.Fatalf("podLive = %s %q, want the layout's reason on the row", live, why)
	}
}

// TestPodWaitNamesARefusedSubPath: the kubelet refusing a folder of the
// pod (a symlink out of its volume) ends the wait with what to do.
func TestPodWaitNamesARefusedSubPath(t *testing.T) {
	d, _ := LoadDeadlines(func(string) string { return "" })
	w := newPodWait(d, DevContainer, "the instance pod")
	err := w.observe(time.Now(), waitPod(waiting("CreateContainerConfigError", `failed to prepare subPath for volumeMount "share" of container "dev"`)), nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), "kubernetes:misconfigured:subpath") {
		t.Fatalf("observe = %v, want the subPath refusal named", err)
	}
	if err := w.observe(time.Now(), waitPod(waiting("CreateContainerConfigError", `secret "x" not found`)), nil); err != nil {
		t.Fatalf("another config error is not a subPath one: %v", err)
	}
}

// TestStartDeadlineNamesTheMountTrouble: a pod stuck on its volumes when its
// start deadline runs out says what the kubelet said (a volume attached to
// another node, an SELinux label another pod mounted it with).
func TestStartDeadlineNamesTheMountTrouble(t *testing.T) {
	pod := waitPod(waiting("ContainerCreating", ""))
	pod.Namespace, pod.UID, pod.Spec.NodeName = handleNS, "pod-uid", "node-a"
	ev := func(name, reason, msg string, at time.Time) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: handleNS}, Reason: reason, Message: msg, LastTimestamp: metav1.NewTime(at),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, UID: pod.UID}}
	}
	now := time.Now()
	cs := fake.NewClientset(
		ev("e1", "FailedMount", "MountVolume.SetUp failed: old", now.Add(-time.Minute)),
		ev("e2", "FailedAttachVolume", `Multi-Attach error for volume "pv-share": Volume is already used by pod(s) fi-a-two-0`, now),
	)
	err := explainMount(context.Background(), cs, pod, &DeadlineError{Kind: CodeTimeout, Wait: "the instance pod to start", Detail: "ContainerCreating"})
	if err == nil || !strings.Contains(err.Error(), "Multi-Attach error") || strings.Contains(err.Error(), "old") {
		t.Fatalf("explainMount = %v, want the latest mount trouble", err)
	}
	other := &DeadlineError{Kind: CodeUnschedulable, Detail: "x"}
	if got := explainMount(context.Background(), cs, pod, other); got != other || other.Detail != "x" {
		t.Fatalf("explainMount changed a deadline that is not the start's: %v", got)
	}
	if mountTrouble(context.Background(), cs, waitPod(nil)) != "" {
		t.Fatal("an unscheduled pod has mount trouble")
	}
}
