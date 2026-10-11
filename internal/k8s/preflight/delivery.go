//go:build !nok8s

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// delivery.go is how built images reach the connection: the image_delivery
// check of every full run (delivery resolved for the nodes the run found and
// checked from the daemon side: the registry, or a manifest fetched through
// the gateway's public URL), and the deep test's image_pull, in which a
// throwaway pod pulls a tiny image through the gateway, so the cluster's own
// nodes prove the route, their trust in the gateway's certificate, the pull
// credential and the digest.

// Delivery is how the daemon delivers the images it builds to the connection
// (the server's kubeimage.Manager and gateway image server). A Target
// without one skips both checks.
type Delivery interface {
	// Check resolves the connection's delivery for the nodes the run found
	// (local is localcluster.Detect's verdict, platform the nodes' os/arch,
	// "" when unknown or mixed) and checks it from the daemon side.
	Check(ctx context.Context, local localcluster.Cluster, platform string) DeliveryCheck
	// PullProbe readies the probe image for a pod in the namespace to pull
	// through the gateway: its reference and its pull Secret (applied).
	PullProbe(ctx context.Context, platform string) (ref, pullSecret string, err error)
	// ClassifyPull names the step a pod's failed pull of ref broke (route,
	// tls, credential or digest; "" when it cannot tell), what happened and
	// the fix, from the waiting container's reason and message.
	ClassifyPull(ref, reason, message string) (step, detail, fix string)
}

// DeliveryCheck is what Delivery.Check found.
type DeliveryCheck struct {
	// Delivery is the connection's KubeCapabilities.image_delivery.
	Delivery *fleetgrpc.KubeImageDelivery
	// Result, Detail and Fix are the image_delivery check's.
	Result      fleetgrpc.KubeCheckResult
	Detail, Fix string
	// Limited, when set, makes the connection LIMITED for this reason: a
	// built image could not reach it.
	Limited string
}

// Timeouts of the delivery checks.
const (
	deliveryTimeout = 45 * time.Second
	// DeepTimeout bounds the deep test's probes, on top of FullTimeout: the
	// pull through the gateway, then the network probe.
	DeepTimeout = 5 * time.Minute
	// pullTimeout bounds the probe pod's pull.
	pullTimeout = 2 * time.Minute
)

// pullPoll is how often the probe pod is looked at (tests shorten it).
var pullPoll = time.Second

// nodePlatform is the nodes' one os/arch, "" when they are unknown or mixed.
func nodePlatform(nodes []corev1.Node) string {
	plats := map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		os, arch := n.Status.NodeInfo.OperatingSystem, n.Status.NodeInfo.Architecture
		if os == "" {
			os = n.Labels[corev1.LabelOSStable]
		}
		if os == "" {
			os = "linux"
		}
		if arch == "" {
			arch = n.Labels[corev1.LabelArchStable]
		}
		if arch == "" {
			return ""
		}
		plats[os+"/"+arch] = true
	}
	if len(plats) != 1 {
		return ""
	}
	for p := range plats {
		return p
	}
	return ""
}

func (r *run) imageDelivery(ctx context.Context) {
	if r.t.Delivery == nil {
		return
	}
	r.running(CheckImageDelivery)
	dctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	dc := r.t.Delivery.Check(dctx, r.res.Local, r.res.Platform)
	r.caps.ImageDelivery = dc.Delivery
	r.emit(&fleetgrpc.KubeCheck{Name: CheckImageDelivery, Result: dc.Result, Detail: dc.Detail, Fix: dc.Fix})
	if dc.Limited != "" {
		r.limit(dc.Limited)
	}
}

// deep runs the throwaway-pod probes: a pull through the gateway, the pods'
// network (network.go), and one RWO volume from two pods (share.go).
func (r *run) deep(ctx context.Context) {
	sharePods := r.startSharePods(ctx)
	r.imagePull(ctx)
	r.networkProbe(ctx)
	sharePods()
	r.res.DeepCheckedAt = r.t.now()
}

// imagePull has a pod pull the probe image through the gateway, when
// delivery resolved to it.
func (r *run) imagePull(ctx context.Context) {
	if r.t.Delivery == nil {
		return
	}
	switch mode := r.caps.GetImageDelivery().GetMode(); {
	case mode != "gateway":
		if mode == "" {
			mode = "unresolved"
		}
		r.skip(CheckImagePull, "delivery is "+mode+": no pull through the gateway to test")
		return
	case !r.nsExists:
		r.skip(CheckImagePull, "namespace missing")
		return
	case r.t.Cluster.ID == "":
		r.skip(CheckImagePull, "save the connection to test a pull through the gateway")
		return
	}
	r.running(CheckImagePull)
	ref, secret, err := r.t.Delivery.PullProbe(ctx, r.res.Platform)
	if err != nil {
		r.fail(CheckImagePull, "the probe image could not be served: "+err.Error(), "")
		r.limit("gateway pull failing")
		return
	}
	pod, err := r.pullPod(ref, secret)
	if err != nil {
		r.fail(CheckImagePull, err.Error(), "")
		return
	}
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	cctx, cancel := call(ctx)
	_, err = pods.Create(cctx, pod, metav1.CreateOptions{})
	cancel()
	if err != nil {
		r.warn(CheckImagePull, "the pull pod could not be created: "+Message(err), "")
		return
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		zero := int64(0)
		_ = pods.Delete(dctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}()

	start := r.t.now()
	deadline := time.Now().Add(pullTimeout)
	lastWait := "waiting to be scheduled"
	for {
		gctx, cancel := call(ctx)
		got, err := pods.Get(gctx, pod.Name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if pulled(got) {
				r.pass(CheckImagePull, fmt.Sprintf("a pod pulled the probe image through the gateway in %s", roundSeconds(r.t.now().Sub(start))))
				return
			}
			if w := waitingOf(got); w != nil {
				lastWait = w.Reason
				if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
					step, msg, fix := r.t.Delivery.ClassifyPull(ref, w.Reason, w.Message)
					detail := msg
					if step != "" {
						detail = step + ": " + msg
					}
					r.fail(CheckImagePull, detail, fix)
					reason := "gateway pull failing"
					if step != "" {
						reason += " (" + step + ")"
					}
					r.limit(reason)
					return
				}
			} else if unsched := unschedulable(got); unsched != "" {
				lastWait = "unschedulable: " + unsched
			}
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			r.warn(CheckImagePull, "the pull pod did not pull within "+pullTimeout.String()+" ("+lastWait+")", "check the pod's events in namespace "+r.t.Cluster.Namespace)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(pullPoll):
		}
	}
}

// pulled reports a probe pod whose image is on its node: its container ran,
// failed to start, or was created (the image has nothing to run).
func pulled(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.ImageID != "" || cs.State.Running != nil || cs.State.Terminated != nil {
			return true
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CreateContainerError", "CreateContainerConfigError", "RunContainerError", "CrashLoopBackOff", "StartError":
				return true
			}
		}
	}
	return false
}

func waitingOf(p *corev1.Pod) *corev1.ContainerStateWaiting {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting
		}
	}
	return nil
}

func unschedulable(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c.Message
		}
	}
	return ""
}

func roundSeconds(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	return d.Round(time.Second).String()
}

// pullPod is the deep test's pull pod: the probe image, pulled Always (a
// node that has it from an earlier test still asks the gateway for its
// manifest), placed like the connection's instances, hardened, and shaped
// for a restricted namespace. It never runs anything: the image's command
// is a text file.
func (r *run) pullPod(ref, secret string) (*corev1.Pod, error) {
	c := r.t.Cluster
	no, yes := false, true
	nobody := int64(65534)
	grace := int64(0)
	deadline := int64(pullTimeout/time.Second) + 60
	labels := names.Base(r.t.Owner, c.ID)
	labels[names.LabelRole] = string(names.RoleProbe)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName(), Namespace: c.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline,
			NodeSelector:                  c.NodeSelector,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &nobody, RunAsGroup: &nobody,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "pull",
				Image:           ref,
				ImagePullPolicy: corev1.PullAlways,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, RunAsNonRoot: &yes,
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if secret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secret}}
	}
	for _, t := range c.Tolerations {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	if err := podspec.Harden(&pod.ObjectMeta, &pod.Spec, podspec.HardenOptions{PinnedImages: true}); err != nil {
		return nil, fmt.Errorf("the pull pod: %w", err)
	}
	return pod, nil
}

func probePodName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "fleet-probe-pull-" + hex.EncodeToString(b[:])
}
