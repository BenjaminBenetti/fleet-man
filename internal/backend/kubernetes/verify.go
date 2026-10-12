//go:build !nok8s

package kubernetes

import (
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
)

// CodeTampered is the error code of a pod that is not the one fleet made.
const CodeTampered = "kubernetes:tampered"

// DefaultFieldManager is the field manager fleet applies its objects with:
// a StatefulSet fleet created lists it in its managedFields.
const DefaultFieldManager = podspec.FieldManager

// DevContainer is the instance pod's container name.
const DevContainer = podspec.DevContainer

// Expect is what fleetd recorded about an instance pod, which the live pod
// must match before it gets a session, a file or a command.
type Expect struct {
	// ImageDigests are the image fleetd delivered (its config digest for a
	// node-loaded image, the manifest digest for a pulled one; any form
	// carrying sha256:<hex>): the dev container must run one of them.
	ImageDigests []string
	// SpecHash is the spec hash of fleetd's own render of the pod
	// (PoolOptions.SpecHash of the StatefulSet template's spec), from its
	// journal. The pod's annotation is never trusted.
	SpecHash string
	// StatefulSetUID is the UID of the StatefulSet fleetd created: the pod's
	// controller must be it.
	StatefulSetUID types.UID
	// FieldManager is the manager the StatefulSet's managedFields must name
	// (DefaultFieldManager when empty).
	FieldManager string
	// DockerImage is the image fleet rendered for the docker engine's
	// sidecar (privileged root beside the dev container), "" when it
	// rendered none or has no record of it: the pod's sidecar must name it,
	// and once it runs, carry its digest when the reference is pinned (the
	// spec hash leaves images out).
	DockerImage string
}

// TamperedError is a pod that is not the one fleet made: another image, a
// changed or substituted spec, or another controller.
type TamperedError struct {
	Namespace, Pod string
	Reasons        []string
}

func (e *TamperedError) Error() string {
	return fmt.Sprintf("%s: pod %s/%s is not the pod fleet made: %s", CodeTampered, e.Namespace, e.Pod, strings.Join(e.Reasons, "; "))
}

// Code is the error's kubernetes: code.
func (e *TamperedError) Code() string { return CodeTampered }

// Errors a verification ends with when the pod is not ready for a session
// yet, or fleetd has nothing to verify it against.
var (
	ErrPodNotReady  = errors.New("the instance pod is not running")
	ErrUnverifiable = errors.New("no record of the instance pod to verify it against")
)

// VerifyPod checks pod is the pod fleetd made: its controller is the
// recorded StatefulSet sts (itself applied by fleet's field manager), its spec
// hashes (with hash) to the recorded render's, and its dev container runs a
// delivered image. It returns a *TamperedError naming every mismatch,
// ErrPodNotReady (wrapped) for a pod that matches but does not run yet, or
// nil.
func VerifyPod(pod *corev1.Pod, sts *appsv1.StatefulSet, want Expect, hash func(*corev1.PodSpec) string) error {
	if want.SpecHash == "" || want.StatefulSetUID == "" || len(want.ImageDigests) == 0 || hash == nil {
		return ErrUnverifiable
	}
	manager := want.FieldManager
	if manager == "" {
		manager = DefaultFieldManager
	}
	var reasons []string
	if sts.UID != want.StatefulSetUID {
		reasons = append(reasons, fmt.Sprintf("StatefulSet %s has uid %s, fleet created %s", sts.Name, sts.UID, want.StatefulSetUID))
	}
	if !managedBy(sts.ManagedFields, manager) {
		reasons = append(reasons, fmt.Sprintf("StatefulSet %s was not applied by fleet (no %q field manager)", sts.Name, manager))
	}
	switch ref := metav1.GetControllerOf(pod); {
	case ref == nil:
		reasons = append(reasons, "it has no controller")
	case ref.Kind != "StatefulSet" || ref.Name != sts.Name || ref.UID != want.StatefulSetUID:
		reasons = append(reasons, fmt.Sprintf("its controller is %s %s (uid %s), not fleet's StatefulSet %s", ref.Kind, ref.Name, ref.UID, sts.Name))
	}
	if got := hash(&pod.Spec); got != want.SpecHash {
		reasons = append(reasons, "its spec differs from fleet's render")
	}
	status := containerStatus(pod, DevContainer)
	if status != nil && status.ImageID != "" && !digestIn(status.ImageID, want.ImageDigests) {
		reasons = append(reasons, fmt.Sprintf("its %s container runs %s, not the image fleet delivered", DevContainer, status.ImageID))
	}
	// The share's init container runs the delivered image too.
	if s := initContainerStatus(pod, podspec.ShareInit); s != nil && s.ImageID != "" && !digestIn(s.ImageID, want.ImageDigests) {
		reasons = append(reasons, fmt.Sprintf("its %s container runs %s, not the image fleet delivered", podspec.ShareInit, s.ImageID))
	}
	reasons = append(reasons, dockerSidecarReasons(pod, want.DockerImage)...)
	if len(reasons) > 0 {
		return &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: reasons}
	}
	switch {
	case pod.DeletionTimestamp != nil:
		return fmt.Errorf("%w: pod %s/%s is terminating", ErrPodNotReady, pod.Namespace, pod.Name)
	case pod.Status.Phase != corev1.PodRunning:
		return fmt.Errorf("%w: pod %s/%s is %s", ErrPodNotReady, pod.Namespace, pod.Name, pod.Status.Phase)
	case status == nil || status.State.Running == nil || status.ImageID == "":
		return fmt.Errorf("%w: pod %s/%s's %s container is not running", ErrPodNotReady, pod.Namespace, pod.Name, DevContainer)
	}
	return nil
}

// dockerImageOf is the image of the docker engine's sidecar o's pod runs, ""
// without one.
func dockerImageOf(o *podspec.Objects) string {
	for _, c := range o.StatefulSet.Spec.Template.Spec.InitContainers {
		if c.Name == podspec.DockerSidecar {
			return c.Image
		}
	}
	return ""
}

// dockerSidecarReasons are how pod's docker engine sidecar is not the one
// fleet rendered with image want ("" checks nothing): another image named,
// or (pinned by digest) another one running.
func dockerSidecarReasons(pod *corev1.Pod, want string) []string {
	if want == "" {
		return nil
	}
	var spec *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == podspec.DockerSidecar {
			spec = &pod.Spec.InitContainers[i]
		}
	}
	switch {
	case spec == nil:
		return []string{fmt.Sprintf("it has no %s container", podspec.DockerSidecar)}
	case spec.Image != want:
		return []string{fmt.Sprintf("its %s container names image %s, not %s fleet rendered", podspec.DockerSidecar, spec.Image, want)}
	}
	pinned := imageDigest(want)
	if s := initContainerStatus(pod, podspec.DockerSidecar); pinned != "" && s != nil && s.ImageID != "" && imageDigest(s.ImageID) != pinned {
		return []string{fmt.Sprintf("its %s container runs %s, not %s", podspec.DockerSidecar, s.ImageID, pinned)}
	}
	return nil
}

func managedBy(fields []metav1.ManagedFieldsEntry, manager string) bool {
	for _, f := range fields {
		if f.Manager == manager {
			return true
		}
	}
	return false
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// initContainerStatus is the status of pod's init container name (a native
// sidecar, the docker engine's), nil without one.
func initContainerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == name {
			return &pod.Status.InitContainerStatuses[i]
		}
	}
	return nil
}

// imageDigest is the sha256:<hex> an image reference or a container
// status's imageID carries ("" when none): imageIDs come as
// repo@sha256:<hex> (a pulled image), sha256:<hex> (a node-loaded one), or
// with a runtime's scheme in front (docker-pullable://...).
func imageDigest(ref string) string {
	i := strings.LastIndex(ref, "sha256:")
	if i < 0 {
		return ""
	}
	d := ref[i:]
	if len(d) != len("sha256:")+64 || !isHex(d[len("sha256:"):]) {
		return ""
	}
	return d
}

func digestIn(imageID string, want []string) bool {
	got := imageDigest(imageID)
	if got == "" {
		return false
	}
	for _, w := range want {
		if imageDigest(w) == got {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
