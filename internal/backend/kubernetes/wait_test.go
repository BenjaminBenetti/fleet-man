//go:build !nok8s

package kubernetes

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func waitPod(mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fi-a-one-aaaaaaaa-0"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: DevContainer}}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func waiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: DevContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
	}
}

func unschedulableBecause(msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: msg}}
	}
}

func pvcIn(phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	class := "standard"
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fi-a-one-aaaaaaaa"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class}, Status: corev1.PersistentVolumeClaimStatus{Phase: phase}}
}

// The deadlines table: each stuck wait fails at its own deadline with its
// own code, and time spent stuck on the disk, the scheduler or the pull is
// not charged to the pod's start.
func TestPodWaitDeadlines(t *testing.T) {
	d, _ := LoadDeadlines(func(string) string { return "" })
	for _, tc := range []struct {
		name string
		// look returns the pod and disk at minute m.
		look   func(m int) (*corev1.Pod, *corev1.PersistentVolumeClaim)
		failAt int // minute it fails at (-1: never within 30)
		code   string
		detail string
	}{
		{"disk never binds", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			return waitPod(unschedulableBecause("0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims.")), pvcIn(corev1.ClaimPending)
		}, 5, CodePVCPending, "storage class standard"},
		{"another claim never binds", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			return waitPod(unschedulableBecause("0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims.")), pvcIn(corev1.ClaimBound)
		}, 3, CodeUnschedulable, "unbound immediate PersistentVolumeClaims"},
		{"never scheduled", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			return waitPod(unschedulableBecause("0/3 nodes are available: 3 Insufficient memory.")), pvcIn(corev1.ClaimBound)
		}, 3, CodeUnschedulable, "Insufficient memory"},
		{"image never pulls", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			return waitPod(waiting("ImagePullBackOff", "Back-off pulling image")), pvcIn(corev1.ClaimBound)
		}, 15, CodeImagePull, "ImagePullBackOff"},
		{"pull then a slow start", func(m int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			if m < 12 {
				return waitPod(waiting("ErrImagePull", "")), nil
			}
			return waitPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning; waiting("ContainerCreating", "")(p) }), nil
		}, 22, CodeTimeout, "ContainerCreating"},
		{"no pod ever", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) { return nil, nil }, 10, CodeTimeout, "no pod was created"},
		{"crash loop", func(int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			return waitPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: DevContainer, RestartCount: 4,
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
			}), nil
		}, 0, CodeOOMKilled, "OOMKilled"},
		{"bound and scheduled in time", func(m int) (*corev1.Pod, *corev1.PersistentVolumeClaim) {
			if m < 4 {
				return waitPod(unschedulableBecause("pod has unbound immediate PersistentVolumeClaims")), pvcIn(corev1.ClaimPending)
			}
			return waitPod(waiting("ContainerCreating", "")), pvcIn(corev1.ClaimBound)
		}, 14, CodeTimeout, "ContainerCreating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWait(d, DevContainer, "the instance pod")
			start := time.Unix(1700000000, 0)
			for m := 0; m <= 30; m++ {
				pod, pvc := tc.look(m)
				err := w.observe(start.Add(time.Duration(m)*time.Minute), pod, pvc)
				if err == nil {
					continue
				}
				var coded interface{ Code() string }
				if m != tc.failAt || !errors.As(err, &coded) || coded.Code() != tc.code || !strings.Contains(err.Error(), tc.detail) {
					t.Fatalf("minute %d: %v, want %s at minute %d with %q", m, err, tc.code, tc.failAt, tc.detail)
				}
				return
			}
			if tc.failAt >= 0 {
				t.Fatalf("never failed, want %s at minute %d", tc.code, tc.failAt)
			}
		})
	}
}

func TestPodReady(t *testing.T) {
	ready := waitPod(func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	if !podReady(ready) || podReady(waitPod(nil)) || podReady(nil) {
		t.Fatal("podReady")
	}
	now := metav1.Now()
	ready.DeletionTimestamp = &now
	if podReady(ready) {
		t.Fatal("a terminating pod is ready")
	}
}
