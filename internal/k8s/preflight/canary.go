//go:build !nok8s

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/serviceimages"
)

// canary.go asks the namespace's admission (Pod Security, policy webhooks,
// quota, LimitRange) what it would do to fleet's pods, with server-side
// dry-runs that create nothing. The canaries are generic: the shape of an
// instance pod as this connection's settings render it (resources,
// placement, runtime class, service account), not any one devcontainer's.
// The real pod is dry-run again at create time.

// canaryImage is the image the canaries name. A dry-run pulls nothing, so it
// only has to be a valid reference; a registry allowlist policy may still
// judge it.
const canaryImage = "registry.k8s.io/pause:3.10"

// shape is one way fleet's pods ask for privilege.
type shape int

const (
	shapeUnprivileged shape = iota // a root container with the default capabilities
	shapeDebug                     // plus SYS_PTRACE and unconfined seccomp
	shapePrivileged                // privileged: true
	shapeRestricted                // non-root, every capability dropped
	shapeBuildkit                  // the rootless Buildkit server
)

// verdict is one canary's outcome.
type verdict struct {
	admitted bool
	// denial is admission's reason, when it refused.
	denial string
	// err is a failure that says nothing about admission (unreachable,
	// forbidden to create, namespace missing).
	err error
}

func (r *run) admission(ctx context.Context) {
	if !r.nsExists {
		for _, name := range []string{CheckAdmissionUnprivileged, CheckAdmissionDebug, CheckAdmissionPrivileged, CheckBuildkit} {
			r.skip(name, "namespace missing")
		}
		return
	}
	adm := &fleetgrpc.KubeAdmission{}
	r.caps.Admission = adm
	allowed := r.caps.PrivilegedAllowed

	// Unprivileged: the instance pod, as a Pod and as the controllers that
	// own it (a policy may judge those directly).
	r.running(CheckAdmissionUnprivileged)
	unpriv := r.dryRunAll(ctx, shapeUnprivileged)
	if unpriv.err != nil {
		r.canaryError(CheckAdmissionUnprivileged, unpriv.err)
		return
	}
	adm.Unprivileged = unpriv.admitted
	if unpriv.admitted {
		r.caps.AudioCapable = true
		r.pass(CheckAdmissionUnprivileged, "")
	} else {
		adm.Denial = unpriv.denial
		restricted := r.dryRunPod(ctx, shapeRestricted)
		if restricted.admitted {
			r.warn(CheckAdmissionUnprivileged, "root containers denied ("+unpriv.denial+"): instances run as one non-root user, no apt or sudo", "a namespace at the baseline pod-security level or looser")
			r.limit("restricted: no root (experimental)")
		} else {
			msg := "the namespace admits no instance pod: " + unpriv.denial
			r.fail(CheckAdmissionUnprivileged, unpriv.denial, "a namespace whose admission allows pods running as root (pod-security baseline)")
			r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg)
		}
	}

	r.running(CheckAdmissionDebug)
	debug := r.dryRunPod(ctx, shapeDebug)
	adm.DebugCapabilities = debug.admitted
	switch {
	case debug.err != nil:
		r.warn(CheckAdmissionDebug, Message(debug.err), "")
	case debug.admitted:
		r.pass(CheckAdmissionDebug, "")
	case r.t.Cluster.DebugCapabilities == fleet.KubeDebugCapabilitiesRequire:
		r.warn(CheckAdmissionDebug, "SYS_PTRACE / unconfined seccomp denied ("+debug.denial+") → creates that ask for them fail (debug capabilities: require)", "set debug capabilities to auto to drop them with a warning")
		r.limit("debug caps denied")
	default:
		r.warn(CheckAdmissionDebug, "SYS_PTRACE / unconfined seccomp denied ("+debug.denial+") → dropped with a warning", "")
		r.limit("debug caps denied")
	}

	r.running(CheckAdmissionPrivileged)
	priv := r.dryRunPod(ctx, shapePrivileged)
	adm.Privileged = priv.admitted
	switch {
	case priv.err != nil:
		r.warn(CheckAdmissionPrivileged, Message(priv.err), "")
	case priv.admitted && allowed:
		r.pass(CheckAdmissionPrivileged, "admitted and allowed")
		r.warning("privileged allowed: the node is the isolation boundary")
	case priv.admitted:
		r.warn(CheckAdmissionPrivileged, "admitted, but Allow privileged pods is off: DinD, DooD and host devices are refused", "turn on Allow privileged pods (the node becomes the isolation boundary)")
		r.limit("privileged not allowed")
	default:
		if adm.Denial == "" {
			adm.Denial = priv.denial
		}
		r.fail(CheckAdmissionPrivileged, priv.denial, "a sysbox runtime class, or a namespace that admits privileged pods")
		r.limit("privileged denied")
	}

	// The shared Buildkit server runs rootless unless privileged pods are
	// allowed, and then as a privileged pod.
	r.running(CheckBuildkit)
	bk := priv
	if !allowed {
		bk = r.dryRunBuildkit(ctx)
	}
	r.caps.BuildkitAdmitted = bk.admitted
	switch {
	case bk.err != nil:
		r.warn(CheckBuildkit, Message(bk.err), "")
	case bk.admitted:
		r.pass(CheckBuildkit, "shared Buildkit admitted")
	default:
		r.caps.BuildkitReason = "Shared Buildkit unavailable on this cluster: the namespace's admission denies its pod (" + bk.denial + "). Use a sysbox runtime class or a namespace that allows it."
		r.warn(CheckBuildkit, r.caps.BuildkitReason, "")
	}
}

// canaryError reports a canary that could not be judged at all.
func (r *run) canaryError(name string, err error) {
	state, msg := Classify(err)
	r.fail(name, msg, "")
	r.failState(state, msg)
	for _, rest := range []string{CheckAdmissionDebug, CheckAdmissionPrivileged, CheckBuildkit} {
		if rest != name {
			r.skip(rest, "the unprivileged canary failed")
		}
	}
}

// dryRunAll dry-runs the shape as a Pod, a StatefulSet and a Job; the first
// refusal decides.
func (r *run) dryRunAll(ctx context.Context, s shape) verdict {
	if v := r.dryRunPod(ctx, s); !v.admitted {
		return v
	}
	spec, labels := r.podSpec(s), r.labels()
	cctx, cancel := call(ctx)
	defer cancel()
	replicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: canaryName(), Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec},
		},
	}
	_, err := r.t.Client.AppsV1().StatefulSets(r.t.Cluster.Namespace).Create(cctx, sts, dryRun())
	if v := judge(err); !v.admitted {
		return v
	}
	jobSpec := r.podSpec(s)
	jobSpec.RestartPolicy = corev1.RestartPolicyNever
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: canaryName(), Labels: labels},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: jobSpec}},
	}
	_, err = r.t.Client.BatchV1().Jobs(r.t.Cluster.Namespace).Create(cctx, job, dryRun())
	return judge(err)
}

func (r *run) dryRunPod(ctx context.Context, s shape) verdict {
	cctx, cancel := call(ctx)
	defer cancel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: canaryName(), Labels: r.labels()}, Spec: r.podSpec(s)}
	_, err := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace).Create(cctx, pod, dryRun())
	return judge(err)
}

func (r *run) dryRunBuildkit(ctx context.Context) verdict {
	if v := r.dryRunPod(ctx, shapeBuildkit); !v.admitted {
		return v
	}
	cctx, cancel := call(ctx)
	defer cancel()
	labels := r.labels()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: canaryName(), Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: r.podSpec(shapeBuildkit)},
		},
	}
	_, err := r.t.Client.AppsV1().Deployments(r.t.Cluster.Namespace).Create(cctx, deploy, dryRun())
	return judge(err)
}

func dryRun() metav1.CreateOptions {
	return metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
}

// judge reads a dry-run's error: nil is admitted; a refusal by admission is
// a denial; anything else says nothing about admission.
func judge(err error) verdict {
	switch {
	case err == nil:
		return verdict{admitted: true}
	case apierrors.IsNotFound(err):
		return verdict{err: err} // the namespace is gone
	case IsUnreachable(err), apierrors.IsUnauthorized(err):
		return verdict{err: err}
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "cannot create resource"):
		return verdict{err: err} // RBAC, not admission
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota"):
		// A full ResourceQuota (fleet's own instances may have filled it)
		// refuses even a dry run, but it is judged last: pod security and
		// the policy webhooks already let the pod through. The quota check
		// reports the headroom.
		return verdict{admitted: true}
	case apierrors.IsForbidden(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return verdict{denial: statusMessage(err)}
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return verdict{denial: statusMessage(err)}
	}
	return verdict{err: err}
}

func canaryName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "fleet-preflight-" + hex.EncodeToString(b[:])
}

func (r *run) labels() map[string]string {
	labels := names.Base(r.t.Owner, r.t.Cluster.ID)
	labels[names.LabelRole] = string(names.RoleTool)
	return labels
}

// podSpec renders the shape with the connection's pod settings.
func (r *run) podSpec(s shape) corev1.PodSpec {
	c := r.t.Cluster
	no, yes := false, true
	root := int64(0)
	spec := corev1.PodSpec{
		AutomountServiceAccountToken: &no,
		EnableServiceLinks:           &no,
		RestartPolicy:                corev1.RestartPolicyAlways,
		NodeSelector:                 c.NodeSelector,
		ServiceAccountName:           c.ServiceAccount,
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	if c.RuntimeClass != "" {
		rc := c.RuntimeClass
		spec.RuntimeClassName = &rc
	}
	for _, t := range c.Tolerations {
		spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	container := corev1.Container{
		Name:            "dev",
		Image:           canaryImage,
		Resources:       r.resources(),
		SecurityContext: &corev1.SecurityContext{RunAsUser: &root},
	}
	switch s {
	case shapeDebug:
		container.SecurityContext.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_PTRACE"}}
		container.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	case shapePrivileged:
		container.SecurityContext.Privileged = &yes
	case shapeRestricted:
		uid := int64(1000)
		container.SecurityContext = &corev1.SecurityContext{
			RunAsUser: &uid, RunAsNonRoot: &yes, AllowPrivilegeEscalation: &no,
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	case shapeBuildkit:
		uid := int64(1000)
		// The image the server runs (a registry allowlist judges it):
		// pinned, or the connection's own.
		container.Name, container.Image = "buildkitd", serviceimages.For(&r.t.Cluster, fleet.ServiceBuildkit, false)
		container.SecurityContext = &corev1.SecurityContext{
			RunAsUser: &uid, RunAsGroup: &uid,
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}
	}
	spec.Containers = []corev1.Container{container}
	return spec
}

// resources are an instance pod's requests and its explicit memory limit.
// A value that does not parse (the editor validates them) is left out.
func (r *run) resources() corev1.ResourceRequirements {
	c := r.t.Cluster
	req := corev1.ResourceList{}
	lim := corev1.ResourceList{}
	if q, err := resource.ParseQuantity(orDefault(c.Resources.CPURequest, fleet.DefaultKubeCPURequest)); err == nil {
		req[corev1.ResourceCPU] = q
	}
	if q, err := resource.ParseQuantity(orDefault(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)); err == nil {
		req[corev1.ResourceMemory] = q
	}
	if q, err := resource.ParseQuantity(orDefault(c.MemoryLimit, fleet.DefaultKubeMemoryLimit)); err == nil {
		lim[corev1.ResourceMemory] = q
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
