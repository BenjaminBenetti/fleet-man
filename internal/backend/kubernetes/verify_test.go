//go:build !nok8s

package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// testHash stands in for podspec.SpecHash: the parts of a spec a
// substitution would change.
func testHash(spec *corev1.PodSpec) string {
	data, _ := json.Marshal(struct {
		Volumes             []corev1.Volume
		Containers          []corev1.Container
		InitContainers      []corev1.Container
		EphemeralContainers []corev1.EphemeralContainer
		HostPID             bool
	}{spec.Volumes, spec.Containers, spec.InitContainers, spec.EphemeralContainers, spec.HostPID})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

const (
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherImage = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// instanceObjects is a StatefulSet fleet applied and the pod it made.
func instanceObjects(ns, sts string, podUID types.UID) (*appsv1.StatefulSet, *corev1.Pod) {
	stsUID := types.UID("sts-uid-" + sts)
	set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: sts, Namespace: ns, UID: stsUID,
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}, {Manager: DefaultFieldManager, Operation: metav1.ManagedFieldsOperationApply}},
	}}
	yes := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sts + "-0", Namespace: ns, UID: podUID,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts, UID: stsUID, Controller: &yes}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: DevContainer, Image: "fleet.invalid/x/devcontainer:abc", Command: []string{"/opt/fleet/bin/fleet", "pod-init"}}},
			Volumes:    []corev1.Volume{{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sts}}}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: DevContainer, ImageID: "docker.io/library/devcontainer@" + testDigest,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	return set, pod
}

func expectFor(set *appsv1.StatefulSet, pod *corev1.Pod) Expect {
	return Expect{ImageDigests: []string{testDigest}, SpecHash: testHash(&pod.Spec), StatefulSetUID: set.UID}
}

func TestVerifyPodAcceptsTheRecordedPod(t *testing.T) {
	set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
	want := expectFor(set, pod)
	if err := VerifyPod(pod, set, want, testHash); err != nil {
		t.Fatal(err)
	}
	// Every form an imageID takes.
	for _, id := range []string{testDigest, "docker-pullable://repo@" + testDigest, "registry.example/fleet/x/devcontainer@" + testDigest} {
		pod.Status.ContainerStatuses[0].ImageID = id
		if err := VerifyPod(pod, set, want, testHash); err != nil {
			t.Errorf("imageID %q: %v", id, err)
		}
	}
	// A recorded digest in reference form matches too.
	want.ImageDigests = []string{"registry.example/fleet/x/devcontainer@" + testDigest}
	if err := VerifyPod(pod, set, want, testHash); err != nil {
		t.Errorf("a recorded reference: %v", err)
	}
}

func TestVerifyPodRefusesASubstitutedPod(t *testing.T) {
	cases := map[string]struct {
		mutate func(*appsv1.StatefulSet, *corev1.Pod)
		reason string
	}{
		"another image": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "docker.io/library/evil@" + otherImage
		}, "not the image fleet delivered"},
		"an extra hostPath volume": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}, "spec differs"},
		"a debug container": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox"}})
		}, "spec differs"},
		"another command": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.Spec.Containers[0].Command = []string{"sh", "-c", "steal"}
		}, "spec differs"},
		"no controller": {func(_ *appsv1.StatefulSet, p *corev1.Pod) { p.OwnerReferences = nil }, "no controller"},
		"a foreign controller": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.OwnerReferences[0].UID = "someone-else"
		}, "not fleet's StatefulSet"},
		"a ReplicaSet controller": {func(_ *appsv1.StatefulSet, p *corev1.Pod) {
			p.OwnerReferences[0].Kind = "ReplicaSet"
		}, "not fleet's StatefulSet"},
		"a recreated StatefulSet": {func(s *appsv1.StatefulSet, p *corev1.Pod) {
			s.UID = "recreated"
			p.OwnerReferences[0].UID = "recreated"
		}, "fleet created"},
		"a StatefulSet fleet did not apply": {func(s *appsv1.StatefulSet, _ *corev1.Pod) {
			s.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply"}}
		}, "was not applied by fleet"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
			want := expectFor(set, pod)
			tc.mutate(set, pod)
			err := VerifyPod(pod, set, want, testHash)
			var tampered *TamperedError
			if !errors.As(err, &tampered) || tampered.Code() != CodeTampered {
				t.Fatalf("err %v, want kubernetes:tampered", err)
			}
			if !strings.Contains(err.Error(), tc.reason) || !strings.HasPrefix(err.Error(), CodeTampered+": pod ns/fi-a-b-key-0") {
				t.Fatalf("err %q, want it to say %q", err, tc.reason)
			}
		})
	}
}

// The annotation a pod carries is never what it is checked against.
func TestVerifyPodNeverTrustsTheAnnotation(t *testing.T) {
	set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
	want := expectFor(set, pod)
	pod.Spec.Containers[0].Command = []string{"sh"}
	pod.Annotations = map[string]string{"fleet.dev/spec-hash": testHash(&pod.Spec)}
	set.Annotations = pod.Annotations
	if err := VerifyPod(pod, set, want, testHash); err == nil {
		t.Fatal("a pod that carries its own spec hash was trusted")
	}
}

func TestVerifyPodNotReady(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"pending": func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending; p.Status.ContainerStatuses = nil },
		"creating": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		},
		"no image yet": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "" },
		"terminating":  func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
	} {
		t.Run(name, func(t *testing.T) {
			set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
			want := expectFor(set, pod)
			mutate(pod)
			if err := VerifyPod(pod, set, want, testHash); !errors.Is(err, ErrPodNotReady) {
				t.Fatalf("err %v, want ErrPodNotReady", err)
			}
		})
	}
	// A pod that is both not ready and substituted is reported as tampered.
	set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
	want := expectFor(set, pod)
	pod.Status.Phase = corev1.PodPending
	pod.OwnerReferences = nil
	var tampered *TamperedError
	if err := VerifyPod(pod, set, want, testHash); !errors.As(err, &tampered) {
		t.Fatalf("err %v", err)
	}
}

func TestVerifyPodNeedsARecord(t *testing.T) {
	set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
	full := expectFor(set, pod)
	for name, want := range map[string]Expect{
		"no hash":   {ImageDigests: full.ImageDigests, StatefulSetUID: full.StatefulSetUID},
		"no image":  {SpecHash: full.SpecHash, StatefulSetUID: full.StatefulSetUID},
		"no owner":  {ImageDigests: full.ImageDigests, SpecHash: full.SpecHash},
		"no hasher": full,
	} {
		hash := testHash
		if name == "no hasher" {
			hash = nil
		}
		if err := VerifyPod(pod, set, want, hash); !errors.Is(err, ErrUnverifiable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestImageDigest(t *testing.T) {
	for in, want := range map[string]string{
		testDigest:                          testDigest,
		"repo@" + testDigest:                testDigest,
		"docker://" + testDigest:            testDigest,
		"sha256:abc":                        "",
		"repo:tag":                          "",
		"sha256:" + strings.Repeat("G", 64): "",
	} {
		if got := imageDigest(in); got != want {
			t.Errorf("imageDigest(%q) = %q, want %q", in, got, want)
		}
	}
}
