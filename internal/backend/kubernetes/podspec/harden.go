//go:build !nok8s

package podspec

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Harden applies fleet's pod defaults to every workload it renders (the
// instance pod, its Jobs, the fleet's caches, the deep test's pods):
//
//   - no service account token and no service links: nothing in a pod
//     reaches the API server or learns the namespace's services through it;
//   - seccomp RuntimeDefault for the pod (a container may still opt out, as
//     the debug grants do, where the privilege policy decided it may);
//   - service-mesh injection turned off (Istio, sidecar and ambient,
//     Linkerd, Kuma, Consul): a proxy sidecar would hold every Job open and
//     intercept the instance's traffic;
//   - fsGroupChangePolicy OnRootMismatch, so a volume is re-owned only when
//     its root does not match (OpenShift's SCCs set an fsGroup);
//   - one SELinux level for every pod of a share-group, derived from its h8,
//     so the pods can share its volumes on an enforcing node, and the
//     MountOption relabel where the volumes' CSI driver supports it;
//   - no host namespaces or hostPath volumes unless the workload is allowed
//     them (a privileged instance that asked), and digest-pinned images where
//     the workload requires them (the services fleet runs).
//
// Harden overwrites what it owns and fails, changing nothing, on what it may
// not fix (host access, an unpinned image).
func Harden(meta *metav1.ObjectMeta, spec *corev1.PodSpec, o HardenOptions) error {
	if v := hardenViolations(spec, o); len(v) > 0 {
		return fmt.Errorf("podspec: %s", strings.Join(v, "; "))
	}
	spec.AutomountServiceAccountToken = ptr(false)
	spec.EnableServiceLinks = ptr(false)
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	psc := spec.SecurityContext
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
		psc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	psc.FSGroupChangePolicy = ptr(corev1.FSGroupChangeOnRootMismatch)
	if level := o.level(); level != "" {
		if psc.SELinuxOptions == nil {
			psc.SELinuxOptions = &corev1.SELinuxOptions{}
		}
		psc.SELinuxOptions.Level = level
		// A container's own SELinux options replace the pod's whole, so the
		// level goes on each that has them.
		forEachContainer(spec, func(c *corev1.Container) {
			if c.SecurityContext != nil && c.SecurityContext.SELinuxOptions != nil {
				c.SecurityContext.SELinuxOptions.Level = level
			}
		})
	}
	if o.SELinuxMount {
		psc.SELinuxChangePolicy = ptr(corev1.SELinuxChangePolicyMountOption)
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	for k, v := range meshOptOutAnnotations {
		meta.Annotations[k] = v
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	for k, v := range meshOptOutLabels {
		meta.Labels[k] = v
	}
	return nil
}

// HardenTemplate is Harden on a workload's pod template.
func HardenTemplate(t *corev1.PodTemplateSpec, o HardenOptions) error {
	return Harden(&t.ObjectMeta, &t.Spec, o)
}

// HardenOptions are what Harden needs to know about a workload.
type HardenOptions struct {
	// ShareGroup is the h8 of the fleet whose share-group the pod belongs to:
	// its SELinux level derives from it. "" sets no level (a workload that
	// shares no volume with a fleet's pods).
	ShareGroup string
	// SELinuxLevel overrides the derived level (an OpenShift namespace's
	// assigned MCS level).
	SELinuxLevel string
	// SELinuxMount sets seLinuxChangePolicy MountOption: the volumes' CSI
	// driver mounts with an SELinux context, which spares the recursive
	// relabel. Only where the API server takes it (the SELinuxMount feature).
	SELinuxMount bool
	// AllowHostAccess permits host namespaces and hostPath volumes: a
	// privileged pod that asked for them, on a connection that allows
	// privileged pods.
	AllowHostAccess bool
	// PinnedImages requires every container image to be pinned by digest.
	PinnedImages bool
}

func (o HardenOptions) level() string {
	if o.SELinuxLevel != "" {
		return o.SELinuxLevel
	}
	if o.ShareGroup == "" {
		return ""
	}
	return SELinuxLevel(o.ShareGroup)
}

// The service-mesh injection opt-outs Harden puts on every pod.
var (
	meshOptOutAnnotations = map[string]string{
		"sidecar.istio.io/inject":             "false",
		"linkerd.io/inject":                   "disabled",
		"kuma.io/sidecar-injection":           "disabled",
		"consul.hashicorp.com/connect-inject": "false",
	}
	meshOptOutLabels = map[string]string{
		// Istio reads the pod label ahead of the annotation, and ambient
		// mode reads only its own.
		"sidecar.istio.io/inject": "false",
		"istio.io/dataplane-mode": "none",
	}
)

// SELinuxLevel is the MCS level of every pod of a share-group: two distinct
// categories of c0 to c1023, derived from the group's h8, written as
// container runtimes write theirs ("s0:c12,c345").
func SELinuxLevel(shareGroup string) string {
	sum := sha256.Sum256([]byte("fleet.dev/selinux\x00" + shareGroup))
	a := binary.BigEndian.Uint16(sum[0:2]) % 1024
	b := binary.BigEndian.Uint16(sum[2:4]) % 1023
	if b >= a {
		b++
	}
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("s0:c%d,c%d", a, b)
}

// CheckHardened lists how a pod differs from what Harden makes of it (nil
// when it does not): for the tests of every package that renders a
// workload.
func CheckHardened(meta metav1.ObjectMeta, spec corev1.PodSpec, o HardenOptions) []string {
	v := hardenViolations(&spec, o)
	if a := spec.AutomountServiceAccountToken; a == nil || *a {
		v = append(v, "the service account token is mounted")
	}
	if s := spec.EnableServiceLinks; s == nil || *s {
		v = append(v, "service links are on")
	}
	psc := spec.SecurityContext
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		v = append(v, "the pod's seccomp profile is not RuntimeDefault")
	}
	if p := psc.FSGroupChangePolicy; p == nil || *p != corev1.FSGroupChangeOnRootMismatch {
		v = append(v, "fsGroupChangePolicy is not OnRootMismatch")
	}
	if level := o.level(); level != "" {
		if psc.SELinuxOptions == nil || psc.SELinuxOptions.Level != level {
			v = append(v, "the pod's SELinux level is not "+level)
		}
		forEachContainer(&spec, func(c *corev1.Container) {
			if c.SecurityContext != nil && c.SecurityContext.SELinuxOptions != nil && c.SecurityContext.SELinuxOptions.Level != level {
				v = append(v, "container "+c.Name+"'s SELinux level is not "+level)
			}
		})
	}
	if o.SELinuxMount && (psc.SELinuxChangePolicy == nil || *psc.SELinuxChangePolicy != corev1.SELinuxChangePolicyMountOption) {
		v = append(v, "seLinuxChangePolicy is not MountOption")
	}
	for _, k := range sortedKeys(meshOptOutAnnotations) {
		if meta.Annotations[k] != meshOptOutAnnotations[k] {
			v = append(v, "annotation "+k+" is not "+meshOptOutAnnotations[k])
		}
	}
	for _, k := range sortedKeys(meshOptOutLabels) {
		if meta.Labels[k] != meshOptOutLabels[k] {
			v = append(v, "label "+k+" is not "+meshOptOutLabels[k])
		}
	}
	return v
}

// hardenViolations is what Harden refuses rather than fixes.
func hardenViolations(spec *corev1.PodSpec, o HardenOptions) []string {
	var v []string
	if !o.AllowHostAccess {
		for what, on := range map[string]bool{"hostNetwork": spec.HostNetwork, "hostPID": spec.HostPID, "hostIPC": spec.HostIPC} {
			if on {
				v = append(v, what+" without host access allowed")
			}
		}
		for _, vol := range spec.Volumes {
			if vol.HostPath != nil {
				v = append(v, "hostPath volume "+vol.Name+" without host access allowed")
			}
		}
	}
	if o.PinnedImages {
		forEachContainer(spec, func(c *corev1.Container) {
			if !strings.Contains(c.Image, "@sha256:") {
				v = append(v, "container "+c.Name+"'s image "+c.Image+" is not pinned by digest")
			}
		})
	}
	sort.Strings(v)
	return v
}

// forEachContainer calls fn on every container of spec, init containers
// included.
func forEachContainer(spec *corev1.PodSpec, fn func(*corev1.Container)) {
	for i := range spec.InitContainers {
		fn(&spec.InitContainers[i])
	}
	for i := range spec.Containers {
		fn(&spec.Containers[i])
	}
}
