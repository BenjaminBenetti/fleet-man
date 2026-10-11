//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const volumeLimitReason = "0/1 nodes are available: 1 node(s) exceed max volume count."

func unschedulablePod(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = nil
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: volumeLimitReason}}
	return pod
}

// The waits for an instance's pod and a Job's pod tell their caller what
// the scheduler keeps the pod waiting on, once per reason.
func TestWaitsHearTheScheduler(t *testing.T) {
	h := newJobHarness(t)
	ctx := context.Background()
	d := DefaultDeadlines()
	d.Unschedulable = 100 * time.Millisecond
	var mu sync.Mutex
	var heard []string
	hear := func(_ context.Context, reason string) {
		mu.Lock()
		heard = append(heard, reason)
		mu.Unlock()
	}

	h.seed(unschedulablePod(&corev1.Pod{ObjectMeta: instMeta(names.Pod(rbSTS), "u-pod", "", names.RoleInstance)}))
	_, err := h.pool.WaitReady(ctx, testCluster, rbSTS, WaitOptions{Interval: 5 * time.Millisecond, Deadlines: &d, Pending: hear})
	var de *DeadlineError
	if !errors.As(err, &de) || de.Kind != CodeUnschedulable {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	if len(heard) != 1 || heard[0] != "Unschedulable: "+volumeLimitReason {
		t.Fatalf("heard %q", heard)
	}
	heard = nil
	mu.Unlock()

	h.mu.Lock()
	h.podFor = func(job *batchv1.Job) *corev1.Pod { return unschedulablePod(jobPodOf(job, corev1.PodPending)) }
	h.mu.Unlock()
	h.addJob("fi-a-one-aaaaaaaa-seed-g1", nil)
	_, err = h.pool.RunJob(ctx, JobRun{Cluster: testCluster, Job: "fi-a-one-aaaaaaaa-seed-g1", Spec: rollbackSpec(1), Interval: 5 * time.Millisecond, Deadlines: &d, Scheduling: hear})
	if !errors.As(err, &de) || de.Kind != CodeUnschedulable {
		t.Fatalf("job err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 || heard[0] != "Unschedulable: "+volumeLimitReason {
		t.Fatalf("job heard %q", heard)
	}
}
