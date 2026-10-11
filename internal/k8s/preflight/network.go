//go:build !nok8s

package preflight

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/kubert"
)

// network.go is the deep test's look at the pods' network (and, from its
// client pod, the node's inotify limits a share's watch draws on): whether the
// namespace's network plugin enforces NetworkPolicies (fleet's isolation of
// a fleet's pods and its metadata block are only advice where it does not),
// and whether a pod shaped like fleet's reaches the cloud metadata service at
// 169.254.169.254, under the same egress rules a fleet's pods get while the
// connection blocks metadata. Two throwaway pods: a target that a
// NetworkPolicy closes to every connection, and a client that tries the
// target and the metadata address.

// Check names of the network probe.
const (
	CheckNetworkPolicy = "network_policy"
	CheckMetadata      = "metadata"
)

// probeImage is the network probe's image: busybox (wget, httpd), pinned.
const probeImage = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// metadataAddress is where the clouds serve instance metadata.
const metadataAddress = "169.254.169.254"

// probeTargetPort is the target pod's HTTP port.
const probeTargetPort = 8080

// networkProbeTimeout bounds the whole probe; networkPoll paces it (tests
// shorten it).
var (
	networkProbeTimeout = 2 * time.Minute
	networkPoll         = time.Second
)

// labelProbe tells the probe's pods apart for its NetworkPolicies.
const labelProbe = "fleet.dev/probe"

// networkVerdict is what the client pod found, with its node's inotify
// limits (0 when unread): what a spoke's watch of a share may use, shared by
// every pod of one uid on the node.
type networkVerdict struct {
	metadataReachable bool
	peerReachable     bool
	inotifyInstances  int64
	inotifyWatches    int64
}

// networkProbe runs the probe and reports CheckNetworkPolicy and
// CheckMetadata.
func (r *run) networkProbe(ctx context.Context) {
	switch {
	case !r.nsExists:
		r.skip(CheckNetworkPolicy, "namespace missing")
		r.skip(CheckMetadata, "namespace missing")
		return
	case r.t.Cluster.ID == "":
		r.skip(CheckNetworkPolicy, "save the connection to probe its pods' network")
		r.skip(CheckMetadata, "save the connection to probe its pods' network")
		return
	}
	r.running(CheckNetworkPolicy)
	r.running(CheckMetadata)
	ctx, cancel := context.WithTimeout(ctx, networkProbeTimeout)
	defer cancel()
	block := r.t.Cluster.BlocksMetadata()
	v, err := r.probeNetwork(ctx, block)
	if err != nil {
		r.warn(CheckNetworkPolicy, "the network probe did not finish: "+err.Error(), "")
		r.warn(CheckMetadata, "the network probe did not finish: "+err.Error(), "")
		return
	}
	r.caps.NetworkPolicyEnforced = !v.peerReachable
	r.caps.MetadataReachable = v.metadataReachable
	r.caps.InotifyMaxUserInstances, r.caps.InotifyMaxUserWatches = v.inotifyInstances, v.inotifyWatches
	if v.peerReachable {
		line := "NetworkPolicies are not enforced here: a fleet's pod isolation and metadata block are advisory"
		r.warning(line)
		r.warn(CheckNetworkPolicy, "a pod reached another pod that a NetworkPolicy closes", "a network plugin that enforces NetworkPolicies (Calico, Cilium)")
	} else {
		r.pass(CheckNetworkPolicy, "a NetworkPolicy closed a pod to another")
	}
	onOff := "egress block off"
	if block {
		onOff = "egress block on"
	}
	if v.metadataReachable {
		line := metadataAddress + " reachable from pods (" + onOff + ")"
		r.warning(line)
		fix := "turn on block_metadata for this cluster"
		if block {
			fix = "a network plugin that enforces NetworkPolicies, or the cloud's metadata hardening (IMDSv2 hop limit 1, GKE Workload Identity)"
		}
		r.warn(CheckMetadata, line, fix)
		return
	}
	r.pass(CheckMetadata, metadataAddress+" not reachable from pods ("+onOff+")")
}

// probeNetwork runs the two probe pods (and their policies) and reads what
// the client found; everything it made is deleted before it returns.
func (r *run) probeNetwork(ctx context.Context, block bool) (networkVerdict, error) {
	ns := r.t.Cluster.Namespace
	cs := r.t.Client
	id := probeID()
	targetName, clientName := "fleet-probe-net-"+id+"-target", "fleet-probe-net-"+id+"-client"
	pods := cs.CoreV1().Pods(ns)
	policies := cs.NetworkingV1().NetworkPolicies(ns)
	var made []func(context.Context)
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		for i := len(made) - 1; i >= 0; i-- {
			made[i](dctx)
		}
	}()
	zero := int64(0)
	deletePod := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			_ = pods.Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		}
	}
	deletePolicy := func(name string) func(context.Context) {
		return func(ctx context.Context) { _ = policies.Delete(ctx, name, metav1.DeleteOptions{}) }
	}

	for _, np := range r.probePolicies(id, block) {
		cctx, cancel := call(ctx)
		_, err := policies.Create(cctx, np, metav1.CreateOptions{})
		cancel()
		if err != nil {
			return networkVerdict{}, fmt.Errorf("creating NetworkPolicy %s: %s", np.Name, Message(err))
		}
		made = append(made, deletePolicy(np.Name))
	}

	target, err := r.probePod(targetName, id+"-target", []string{"httpd", "-f", "-v", "-p", fmt.Sprint(probeTargetPort), "-h", "/"})
	if err != nil {
		return networkVerdict{}, err
	}
	cctx, cancel := call(ctx)
	_, err = pods.Create(cctx, target, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return networkVerdict{}, fmt.Errorf("creating the target pod: %s", Message(err))
	}
	made = append(made, deletePod(targetName))
	ip, err := r.waitPodIP(ctx, targetName)
	if err != nil {
		return networkVerdict{}, err
	}

	client, err := r.probePod(clientName, id+"-client", []string{"sh", "-c", probeScript(ip)})
	if err != nil {
		return networkVerdict{}, err
	}
	client.Spec.RestartPolicy = corev1.RestartPolicyNever
	cctx, cancel = call(ctx)
	_, err = pods.Create(cctx, client, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return networkVerdict{}, fmt.Errorf("creating the client pod: %s", Message(err))
	}
	made = append(made, deletePod(clientName))
	if err := r.waitPodDone(ctx, clientName); err != nil {
		return networkVerdict{}, err
	}
	out, err := clientLog(r, ctx, clientName)
	if err != nil {
		return networkVerdict{}, err
	}
	return parseNetworkProbe(out)
}

// probeScript is the client's: one wget at a time, each line naming its
// target and whether it got an answer (any HTTP status counts: the address
// answered).
func probeScript(targetIP string) string {
	return `probe() {
  out=$(wget -q -T 3 -O /dev/null "$2" 2>&1); rc=$?
  case "$out" in *"server returned error"*) rc=0 ;; esac
  if [ "$rc" = 0 ]; then echo "$1=reached"; else echo "$1=unreached"; fi
}
probe metadata http://` + metadataAddress + `/
probe peer http://` + podAddress(targetIP) + `:` + fmt.Sprint(probeTargetPort) + `/
echo "inotify_instances=$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null)"
echo "inotify_watches=$(cat /proc/sys/fs/inotify/max_user_watches 2>/dev/null)"
`
}

// podAddress is ip as a URL host (an IPv6 address in brackets).
func podAddress(ip string) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]"
	}
	return ip
}

// parseNetworkProbe reads the client's output.
func parseNetworkProbe(out string) (networkVerdict, error) {
	var v networkVerdict
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		k, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "inotify_instances", "inotify_watches":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n < 0 {
				continue
			}
			if k == "inotify_instances" {
				v.inotifyInstances = n
			} else {
				v.inotifyWatches = n
			}
			continue
		}
		if val != "reached" && val != "unreached" {
			continue
		}
		seen[k] = true
		switch k {
		case "metadata":
			v.metadataReachable = val == "reached"
		case "peer":
			v.peerReachable = val == "reached"
		}
	}
	if !seen["metadata"] || !seen["peer"] {
		return networkVerdict{}, fmt.Errorf("the client pod's output is not the probe's: %q", firstLines(out, 3))
	}
	return v, nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

// probePolicies are the probe's NetworkPolicies: the target closed to every
// connection, and, while the connection blocks metadata, the client under a
// fleet pod's egress rules.
func (r *run) probePolicies(id string, block bool) []*networkingv1.NetworkPolicy {
	labels := r.probeLabels("")
	out := []*networkingv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-probe-net-" + id + "-closed", Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelProbe: id + "-target"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}}
	if block {
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "fleet-probe-net-" + id + "-egress", Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelProbe: id + "-client"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      kubert.MetadataBlockRules(),
			},
		})
	}
	return out
}

// probeLabels are a probe object's labels; probe tells its pods apart.
func (r *run) probeLabels(probe string) map[string]string {
	labels := names.Base(r.t.Owner, r.t.Cluster.ID)
	labels[names.LabelRole] = string(names.RoleProbe)
	if probe != "" {
		labels[labelProbe] = probe
	}
	return labels
}

// probePod is one of the probe's pods: busybox running command as nobody,
// placed like the connection's instances, hardened, and shaped for a
// restricted namespace.
func (r *run) probePod(name, probe string, command []string) (*corev1.Pod, error) {
	c := r.t.Cluster
	no, yes := false, true
	nobody := int64(65534)
	grace := int64(0)
	deadline := int64(networkProbeTimeout/time.Second) + 60
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: r.probeLabels(probe)},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline,
			NodeSelector:                  c.NodeSelector,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &nobody, RunAsGroup: &nobody,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   probeImage,
				Command: command,
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
	for _, t := range c.Tolerations {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	if err := podspec.Harden(&pod.ObjectMeta, &pod.Spec, podspec.HardenOptions{PinnedImages: true}); err != nil {
		return nil, fmt.Errorf("the probe pod: %w", err)
	}
	return pod, nil
}

// waitPodIP waits for pod name to run and returns its IP.
func (r *run) waitPodIP(ctx context.Context, name string) (string, error) {
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	last := "waiting to be scheduled"
	for {
		gctx, cancel := call(ctx)
		p, err := pods.Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
				return p.Status.PodIP, nil
			}
			if w := waitingOf(p); w != nil {
				last = w.Reason
				if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
					return "", fmt.Errorf("the probe image %s did not pull: %s", probeImage, w.Message)
				}
			} else if u := unschedulable(p); u != "" {
				last = "unschedulable: " + u
			}
		}
		if err := sleepCtx(ctx, networkPoll); err != nil {
			return "", fmt.Errorf("the target pod did not run (%s)", last)
		}
	}
}

// waitPodDone waits for pod name to finish.
func (r *run) waitPodDone(ctx context.Context, name string) error {
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	last := "waiting to be scheduled"
	for {
		gctx, cancel := call(ctx)
		p, err := pods.Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			switch p.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return nil
			}
			if w := waitingOf(p); w != nil {
				last = w.Reason
				if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
					return fmt.Errorf("the probe image %s did not pull: %s", probeImage, w.Message)
				}
			} else if p.Status.Phase == corev1.PodRunning {
				last = "running"
			}
		}
		if err := sleepCtx(ctx, networkPoll); err != nil {
			return fmt.Errorf("the client pod did not finish (%s)", last)
		}
	}
}

// clientLog reads the client pod's log (a seam: a fake cluster's logs are
// fixed).
var clientLog = func(r *run, ctx context.Context, name string) (string, error) { return r.podLog(ctx, name) }

// podLog is pod name's log.
func (r *run) podLog(ctx context.Context, name string) (string, error) {
	cctx, cancel := call(ctx)
	defer cancel()
	rc, err := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace).GetLogs(name, &corev1.PodLogOptions{}).Stream(cctx)
	if err != nil {
		return "", fmt.Errorf("reading the client pod's log: %s", Message(err))
	}
	defer rc.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, io.LimitReader(rc, 64<<10)); err != nil {
		return "", fmt.Errorf("reading the client pod's log: %v", err)
	}
	return b.String(), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func probeID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
