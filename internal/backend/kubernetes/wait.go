//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// wait.go waits for a pod fleet started (an instance's, a Job's) with the
// deadlines (deadlines.go), naming what it was stuck on when one runs out:
// its disk not binding, the scheduler refusing it, its image not pulling,
// or, for everything else, its own start taking too long. Time spent on the
// disk, the scheduler or the pull is not charged to the start.

// PodError is a pod that failed while fleet waited on it (a crash loop
// before it was ever ready).
type PodError struct {
	Kind   string
	Detail string
}

func (e *PodError) Error() string { return e.Kind + ": " + e.Detail }

// Code is the error's kubernetes: code.
func (e *PodError) Code() string { return e.Kind }

// podWait judges, one look at a time, how long a pod has been stuck and on
// what.
type podWait struct {
	d         Deadlines
	container string
	// what names the pod in messages ("the instance pod").
	what string
	last time.Time
	// The stuck-since times of the waits that have their own deadline, and
	// the start time used so far.
	pvcSince, schedSince, pullSince time.Time
	startUsed                       time.Duration
	// blocked: the last look found the pod stuck on a wait with its own
	// deadline, so the time since is not the start's.
	blocked bool
}

func newPodWait(d Deadlines, container, what string) *podWait {
	return &podWait{d: d, container: container, what: what}
}

// observe takes a look at pod (nil: not created yet) and its disk pvc (nil:
// not waited on) at now, and returns the error of a deadline that passed.
func (w *podWait) observe(now time.Time, pod *corev1.Pod, pvc *corev1.PersistentVolumeClaim) error {
	// The time since the last look is charged as that look found the pod.
	var dt time.Duration
	if !w.last.IsZero() && now.After(w.last) && !w.blocked {
		dt = now.Sub(w.last)
	}
	w.last = now
	blocked := false
	defer func() { w.blocked = blocked }()

	diskPending := pvc != nil && pvc.Status.Phase != corev1.ClaimBound
	if diskPending {
		blocked = true
		if w.pvcSince.IsZero() {
			w.pvcSince = now
		}
		if now.Sub(w.pvcSince) >= w.d.PVCBind {
			return &DeadlineError{Kind: CodePVCPending, Wait: fmt.Sprintf("%s's disk (PVC %s) to bind", w.what, pvc.Name), After: w.d.PVCBind, Detail: pvcDetail(pvc), Env: EnvPVCBindTimeout}
		}
	} else {
		w.pvcSince = time.Time{}
	}

	if pod != nil {
		if c := unschedulable(pod); c != nil {
			blocked = true
			// A pod held back for its unbound disk waits on the disk,
			// which has its own deadline; held back for another claim
			// (the disk is bound, or not watched), it waits on the
			// scheduler's.
			if !diskPending || !waitsOnClaims(c.Message) {
				if w.schedSince.IsZero() {
					w.schedSince = now
				}
				if now.Sub(w.schedSince) >= w.d.Unschedulable {
					return &DeadlineError{Kind: CodeUnschedulable, Wait: w.what + " to be scheduled", After: w.d.Unschedulable, Detail: joinReason(c.Reason, c.Message), Env: EnvUnschedulableTimeout}
				}
			}
		} else {
			w.schedSince = time.Time{}
		}
		if err := shareLayoutFailed(pod); err != nil {
			return err
		}
		dev := containerStatus(pod, w.container)
		if err := subPathRefused(pod, dev); err != nil {
			return err
		}
		// The docker engine's sidecar, which the dev container waits for.
		docker := initContainerStatus(pod, podspec.DockerSidecar)
		switch {
		case dev != nil && dev.State.Waiting != nil && pullReason(dev.State.Waiting.Reason):
			blocked = true
			if w.pullSince.IsZero() {
				w.pullSince = now
			}
			if now.Sub(w.pullSince) >= w.d.ImagePull {
				return &DeadlineError{Kind: CodeImagePull, Wait: w.what + "'s image to pull", After: w.d.ImagePull, Detail: waitingReason(dev), Env: EnvImagePullTimeout}
			}
		case docker != nil && docker.State.Waiting != nil && pullReason(docker.State.Waiting.Reason):
			blocked = true
			if w.pullSince.IsZero() {
				w.pullSince = now
			}
			if now.Sub(w.pullSince) >= w.d.ImagePull {
				return &DeadlineError{Kind: CodeImagePull, Wait: "the docker engine's image (the " + podspec.DockerSidecar + " sidecar) to pull", After: w.d.ImagePull, Detail: waitingReason(docker), Env: EnvImagePullTimeout}
			}
		default:
			w.pullSince = time.Time{}
		}
		if dev != nil && dev.State.Waiting != nil && dev.State.Waiting.Reason == "CrashLoopBackOff" {
			l := loopOf(pod, int(dev.RestartCount), true)
			detail := "the dev container keeps exiting before it is ready"
			if l.Reason != "" {
				detail += fmt.Sprintf(" (%s, exit code %d)", l.Reason, l.ExitCode)
			}
			return &PodError{Kind: l.Code(), Detail: detail}
		}
		if docker != nil && docker.State.Waiting != nil && docker.State.Waiting.Reason == "CrashLoopBackOff" {
			detail := fmt.Sprintf("the docker engine (the %s sidecar) keeps exiting before it answers", podspec.DockerSidecar)
			if term := docker.LastTerminationState.Terminated; term != nil {
				detail += fmt.Sprintf(" (%s, exit code %d)", term.Reason, term.ExitCode)
			}
			detail += fmt.Sprintf("; `kubectl logs %s -c %s` says why", pod.Name, podspec.DockerSidecar)
			return &PodError{Kind: CodeCrashLoop, Detail: detail}
		}
	}

	w.startUsed += dt
	if !blocked {
		if w.startUsed >= w.d.PodReady {
			detail := "no pod was created"
			if pod != nil {
				_, detail = podLive(pod)
				if detail == "" {
					detail = "not ready"
				}
			}
			return &DeadlineError{Kind: CodeTimeout, Wait: w.what + " to start", After: w.d.PodReady, Detail: detail, Env: EnvPodReadyTimeout}
		}
	}
	return nil
}

// shareLayoutAttempts is how many times the share-layout init container
// fails before the wait ends: the kubelet runs it again, and a failure that
// passes (the share briefly full, a folder another pod was laying out) must
// not fail a create at once.
const shareLayoutAttempts = 3

// shareLayoutFailed is the share-layout init container failing again and
// again: it lays out the same tree each time, so the reason it gave (its
// termination message) does not go away by waiting more.
func shareLayoutFailed(pod *corev1.Pod) error {
	s := initContainerStatus(pod, podspec.ShareInit)
	if s == nil {
		return nil
	}
	t := s.State.Terminated
	if t == nil {
		t = s.LastTerminationState.Terminated
	}
	if t == nil || t.ExitCode == 0 {
		return nil
	}
	// The kubelet counts the runs after the first.
	if failures := s.RestartCount + 1; failures < shareLayoutAttempts {
		return nil
	}
	why := strings.TrimSpace(t.Message)
	if why == "" {
		why = fmt.Sprintf("%s, exit code %d; `kubectl logs %s -c %s` says why", t.Reason, t.ExitCode, pod.Name, podspec.ShareInit)
	}
	return &PodError{Kind: string(kubeerr.Misconfigured.Narrow("share")), Detail: "the fleet's share could not be laid out for the pod: " + why}
}

// subPathRefused is the kubelet refusing to mount one of the pod's folders
// (CreateContainerConfigError "failed to prepare subPath"): a symlink there
// leaves its volume, which only something in the pod (or another of the
// fleet's) can have made, and which waiting never mends.
func subPathRefused(pod *corev1.Pod, st *corev1.ContainerStatus) error {
	if st == nil || st.State.Waiting == nil || st.State.Waiting.Reason != "CreateContainerConfigError" || !strings.Contains(st.State.Waiting.Message, "subPath") {
		return nil
	}
	return &PodError{Kind: string(kubeerr.Misconfigured.Narrow("subpath")), Detail: fmt.Sprintf(
		"the kubelet will not mount a folder of the pod: %s; a symlink there points out of its volume: remove it (a fleet folder on the share is laid out again at the pod's next start), or destroy the instance",
		joinReason("", st.State.Waiting.Message))}
}

// mountTrouble is the latest of the kubelet's FailedMount and
// FailedAttachVolume events for pod ("" when none): what a pod stuck in
// ContainerCreating waits on (a volume attached elsewhere, an SELinux
// label another pod on the node mounted it with).
func mountTrouble(ctx context.Context, cs kclient.Interface, pod *corev1.Pod) string {
	if pod == nil || pod.Spec.NodeName == "" {
		return ""
	}
	var latest *corev1.Event
	for _, reason := range []string{"FailedMount", "FailedAttachVolume"} {
		evs, err := cs.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + pod.Name + ",reason=" + reason})
		if err != nil {
			continue
		}
		for i := range evs.Items {
			ev := &evs.Items[i]
			if ev.InvolvedObject.UID != "" && pod.UID != "" && ev.InvolvedObject.UID != pod.UID {
				continue
			}
			if latest == nil || eventTime(*ev).After(eventTime(*latest)) {
				latest = ev
			}
		}
	}
	if latest == nil {
		return ""
	}
	return joinReason(latest.Reason, latest.Message)
}

// explainMount adds the kubelet's latest mount trouble to a start deadline
// that ran out on a scheduled pod.
func explainMount(ctx context.Context, cs kclient.Interface, pod *corev1.Pod, err error) error {
	var de *DeadlineError
	if !errors.As(err, &de) || de.Kind != CodeTimeout {
		return err
	}
	if why := mountTrouble(ctx, cs, pod); why != "" {
		de.Detail += "; the kubelet says " + why
	}
	return err
}

// unschedulable is the pod's PodScheduled=False Unschedulable condition.
func unschedulable(pod *corev1.Pod) *corev1.PodCondition {
	for i, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// waitsOnClaims reports a scheduler message that blames unbound volumes.
func waitsOnClaims(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "unbound") && strings.Contains(m, "persistentvolumeclaim")
}

// pullReason reports a waiting reason of an image that does not pull.
func pullReason(r string) bool {
	switch r {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "RegistryUnavailable":
		return true
	}
	return false
}

func pvcDetail(pvc *corev1.PersistentVolumeClaim) string {
	class := "the default storage class"
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		class = "storage class " + *pvc.Spec.StorageClassName
	}
	phase := string(pvc.Status.Phase)
	if phase == "" {
		phase = string(corev1.ClaimPending)
	}
	return fmt.Sprintf("still %s on %s; `kubectl describe pvc %s` says why", phase, class, pvc.Name)
}

// podReady reports a pod whose Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// DefaultWaitInterval is how often a wait looks at the pod.
const DefaultWaitInterval = 2 * time.Second

// WaitOptions configure WaitReady.
type WaitOptions struct {
	// Deadlines are the waits' bounds; nil is DefaultDeadlines().
	Deadlines *Deadlines
	// PVC is the instance's disk, watched for binding ("" watches none).
	PVC string
	// Interval is the time between looks (DefaultWaitInterval).
	Interval time.Duration
	// Check, when set, runs at every look; its error ends the wait (the
	// caller's own judgement, such as a StatefulSet whose pod creation the
	// API refused).
	Check func(ctx context.Context) error
}

// WaitReady waits until the pod of instance StatefulSet sts on connection
// clusterID is Ready and returns it, or fails with a *DeadlineError naming
// what it was stuck on (or a *PodError for a pod that kept crashing).
func (p *Pool) WaitReady(ctx context.Context, clusterID, sts string, opts WaitOptions) (*corev1.Pod, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, err
	}
	d := DefaultDeadlines()
	if opts.Deadlines != nil {
		d = *opts.Deadlines
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultWaitInterval
	}
	ns, podName := c.Namespace(), names.Pod(sts)
	w := newPodWait(d, DevContainer, "the instance pod")
	for {
		objs, err := p.objects(c)
		if err != nil {
			return nil, err
		}
		pod, err := objs.Pod(ctx, ns, podName)
		if err != nil && !gone(err) {
			return nil, fmt.Errorf("reading pod %s/%s: %w", ns, podName, err)
		}
		if err != nil {
			pod = nil
		}
		if podReady(pod) {
			return pod, nil
		}
		var pvc *corev1.PersistentVolumeClaim
		if opts.PVC != "" {
			got, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, opts.PVC, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("reading PVC %s/%s: %w", ns, opts.PVC, err)
			}
			pvc = got
		}
		if err := w.observe(p.opts.Now(), pod, pvc); err != nil {
			return nil, explainMount(ctx, cs, pod, err)
		}
		if opts.Check != nil {
			if err := opts.Check(ctx); err != nil {
				return nil, err
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
