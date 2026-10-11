//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// share.go is how a connection holds its fleets' shares (the volume a
// fleet's agent dirs, shared files and custom mounts live on): the share
// mode and its StorageClass (shared_storage), and the deep test's two-pod
// probe of one ReadWriteOnce volume (share_pods).
//
// A class that passed the multi-node test (storagetest.go) holds shares in
// multi-node mode: the fleet's instances run on any node, sharing one
// ReadWriteMany volume. Auto picks one (the connection's shared class when
// it names one: one that fails the test's file system checks is refused);
// without one, the share is in single-node mode: the fleet's instances run
// together on the node holding it, in the shared class, else the instance
// disks'. When two pods on one node cannot mount one RWO volume (share_pods
// failed), fleets get no share at all: each instance keeps its own copy of
// the fleet's folders (the copy tier), and agent logins may need repeating,
// since copies that each refresh a login race (the P4 refresh spike ruled
// the single-refresher fix unsafe).

// Check names of the share checks.
const (
	CheckSharedStorage = "shared_storage"
	CheckSharePods     = "share_pods"
)

// LimitedCopyTier is the LIMITED reason of a connection on the copy tier.
const LimitedCopyTier = "logins may need repeating"

// LimitedOneNode is the LIMITED reason of a connection of several nodes
// whose fleets each run on one node (single-node mode).
const LimitedOneNode = "each fleet on one node"

// OneNodeWarning is the capability line of single-node mode on a cluster
// of several nodes.
const OneNodeWarning = "Each fleet's instances run together on one node"

// The other LIMITED reasons shared_storage gives.
const (
	limitedSharesUnavailable = "fleet shares unavailable"
	limitedSharesImmediate   = "fleet shares: Immediate binding"
)

// sharePodsTimeout bounds the two-pod probe, sharePodsStart how long its
// first pod (and the volume it binds) may take to run, sharePodsMount how
// long its second pod may take to mount the volume once the first runs
// (tests shorten them). It runs beside the network probe, within its two
// minutes.
var (
	sharePodsTimeout = 150 * time.Second
	sharePodsStart   = time.Minute
	sharePodsMount   = 75 * time.Second
	sharePodsPoll    = 2 * time.Second
)

// shareClass is the class a fleet share takes in single-node mode, as a
// create picks it: the shared class, else the instance disks', else the
// cluster's default. ok is false when the class named is not one the
// listing has.
func (r *run) shareClass() (*fleetgrpc.KubeStorageClass, string, bool) {
	name := r.t.Cluster.SharedStorageClass
	if name == "" {
		name = r.caps.InstanceStorageClass
	}
	return r.listedClass(name)
}

// listedClass is class name as listed ("" the default class); ok false
// when the listing has none such.
func (r *run) listedClass(name string) (*fleetgrpc.KubeStorageClass, string, bool) {
	for _, c := range r.caps.StorageClasses {
		if (name != "" && c.Name == name) || (name == "" && c.IsDefault) {
			return c, c.Name, true
		}
	}
	return nil, name, false
}

// sharedStorage reports the share mode and its class.
func (r *run) sharedStorage(ctx context.Context) {
	r.running(CheckSharedStorage)
	// The deep test's probes run this again with their word: what an
	// earlier evaluation limited the connection by goes, as its check's
	// result does.
	r.unlimit(LimitedCopyTier, limitedSharesUnavailable, limitedSharesImmediate, LimitedOneNode)
	r.res.Warnings = slices.DeleteFunc(r.res.Warnings, func(w string) bool { return w == OneNodeWarning })
	r.caps.ShareRefused, r.caps.SharedStorageClass, r.caps.ShareMode = "", "", ""
	r.testStorage(ctx, false)

	picked := r.t.Cluster.SharedStorageClass
	if picked != "" {
		_, _, listed := r.listedClass(picked)
		t, tested := r.storage[picked]
		switch {
		case !listed && len(r.caps.StorageClasses) > 0:
			msg := fmt.Sprintf("storage class %q for fleet shares does not exist", picked)
			r.fail(CheckSharedStorage, msg, "pick one the cluster offers, or clear the setting for Auto")
			r.limit(limitedSharesUnavailable)
			r.caps.ShareRefused = msg
			return
		case tested && t.Refused():
			msg := fmt.Sprintf("storage class %s fails the multi-node test: %s", picked, t.Detail)
			fix := t.Fix
			if fix == "" {
				fix = "pick a class that passes it, or clear the setting for Auto"
			}
			r.fail(CheckSharedStorage, msg, fix)
			r.limit(limitedSharesUnavailable)
			r.caps.ShareRefused = msg
			return
		case tested && t.Passed():
			r.multiNodeMode(picked, false)
			return
		}
	} else if best := r.bestStorage(); best != "" {
		r.multiNodeMode(best, true)
		return
	}

	if r.multiPodRWO != nil && !*r.multiPodRWO {
		r.caps.ShareMode = string(topology.Copy)
		r.limit(LimitedCopyTier)
		r.warn(CheckSharedStorage, "copy tier: two pods on one node cannot mount one volume here (share_pods), so each instance keeps its own copy of the fleet's folders, synced through the daemon; agent logins may need repeating",
			"a StorageClass whose volumes two pods on one node can mount, for the shared or the instance disk class")
		return
	}
	r.singleNodeMode(ctx)
}

// multiNodeMode holds new fleet shares in class, which passed the
// multi-node test (auto: Auto picked it).
func (r *run) multiNodeMode(class string, auto bool) {
	r.caps.ShareMode = string(topology.MultiNode)
	r.caps.SharedStorageClass = class
	if r.multiPodRWO != nil && *r.multiPodRWO {
		r.caps.MultiPodRwo = true
	}
	traits := []string{"ReadWriteMany"}
	if c, _, ok := r.listedClass(class); ok {
		if c.AllowExpansion {
			traits = append(traits, "expandable")
		} else {
			traits = append(traits, "not expandable: a nearly full share only warns")
		}
	}
	detail := fmt.Sprintf("multi-node mode: each fleet's instances run on any node, sharing its share (%s, %s; passed the multi-node test)", class, strings.Join(traits, ", "))
	if auto {
		detail = "auto → " + class + ": " + detail
	}
	r.pass(CheckSharedStorage, detail)
}

// singleNodeMode holds new fleet shares in single-node mode, in the shared
// class, else the instance disks'.
func (r *run) singleNodeMode(ctx context.Context) {
	r.caps.ShareMode = string(topology.SingleNode)
	if r.multiPodRWO != nil {
		r.caps.MultiPodRwo = true
	}
	picked := r.t.Cluster.SharedStorageClass
	class, name, ok := r.shareClass()
	r.caps.SharedStorageClass = name
	// On a cluster of several nodes, single-node mode is a degradation:
	// say why no class gave multi-node mode.
	why, fix := "", ""
	if r.multiNode {
		r.limit(LimitedOneNode)
		r.warning(OneNodeWarning)
		why, fix = r.whyNotMultiNode(picked)
	}
	if !ok {
		what := "the cluster's default class"
		if name != "" {
			what = "class " + name
		}
		detail := "single-node mode: each fleet's instances run together on the node holding its share (" + what + ")"
		if why != "" {
			r.warn(CheckSharedStorage, why+"; "+detail, fix)
			return
		}
		r.pass(CheckSharedStorage, detail)
		return
	}
	traits := []string{bindingLabel(class.BindingMode)}
	if class.NodeLocal {
		traits = append(traits, "node-local")
	}
	if class.AllowExpansion {
		traits = append(traits, "expandable")
	} else {
		traits = append(traits, "not expandable: a nearly full share only warns")
	}
	detail := fmt.Sprintf("single-node mode: each fleet's instances run together on the node holding its share (%s, %s)", class.Name, strings.Join(traits, ", "))
	if picked == "" && why != "" {
		detail = "auto → " + class.Name + ": " + detail
	}
	tc := topology.Class{Name: class.Name, Immediate: class.BindingMode == string(storagev1.VolumeBindingImmediate), NodeLocal: class.NodeLocal}
	if tc.Immediate {
		cctx, cancel := call(ctx)
		if sc, err := r.t.Client.StorageV1().StorageClasses().Get(cctx, class.Name, metav1.GetOptions{}); err == nil {
			tc.AllowedTopologies = sc.AllowedTopologies
		}
		cancel()
	}
	if problem := topology.ImmediateProblem(tc, len(r.caps.Nodes)); problem != "" {
		if why != "" {
			detail = why + "; " + detail
		}
		r.warn(CheckSharedStorage, detail+": "+problem, topology.ImmediateFix)
		r.limit(limitedSharesImmediate)
		return
	}
	if why != "" {
		r.warn(CheckSharedStorage, why+"; "+detail, fix)
		return
	}
	r.pass(CheckSharedStorage, detail)
}

// whyNotMultiNode says why no class gave multi-node mode on a cluster of
// several nodes, with the remedy.
func (r *run) whyNotMultiNode(picked string) (string, string) {
	if picked != "" {
		if t, ok := r.storage[picked]; ok {
			return fmt.Sprintf("%s does not give multi-node mode: %s", picked, t.Detail), t.Fix
		}
		if c, _, ok := r.listedClass(picked); ok && (c.NodeLocal || blockOnlyProvisioners[c.Provisioner]) {
			return fmt.Sprintf("%s does not give multi-node mode: its volumes mount on one node at a time", picked), "a ReadWriteMany class that passes the multi-node test, or clear the setting for Auto"
		}
		return fmt.Sprintf("%s is not tested yet for the multi-node test", picked), ""
	}
	var failed, unsure, untested []string
	for _, c := range r.caps.StorageClasses {
		name := c.GetName()
		if t, ok := r.storage[name]; ok {
			if t.Inconclusive {
				unsure = append(unsure, name)
			} else {
				failed = append(failed, name)
			}
			continue
		}
		if storageSkip(c, true, false) == "" {
			untested = append(untested, name)
		}
	}
	var parts []string
	switch {
	case len(failed)+len(unsure) == 0 && len(untested) == 0:
		parts = append(parts, fmt.Sprintf("no class can pass the multi-node test (%d block or node-local)", len(r.caps.StorageClasses)))
	case len(failed)+len(unsure) == 0:
		parts = append(parts, "no class passed the multi-node test")
	default:
		parts = append(parts, "no class passed the multi-node test")
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, ", "))
	}
	if len(unsure) > 0 {
		parts = append(parts, "could not tell: "+strings.Join(unsure, ", "))
	}
	if len(untested) > 0 {
		what := "not tested yet: " + strings.Join(untested, ", ")
		if r.t.StorageCache == nil || r.t.Cluster.ID == "" {
			what = "not tested (save the connection to test them): " + strings.Join(untested, ", ")
		}
		parts = append(parts, what)
	}
	return strings.Join(parts, " · "), noClassFix(r.caps.Nodes, r.providers)
}

// bestStorage is Auto's shared class: of the classes that passed the
// multi-node test, the cluster's default, then an expandable one, then by
// name ("" when none passed).
func (r *run) bestStorage() string {
	best, bestScore := "", -1
	for _, c := range r.caps.StorageClasses {
		t, ok := r.storage[c.GetName()]
		if !ok || !t.Passed() {
			continue
		}
		score := 0
		if c.GetIsDefault() {
			score += 2
		}
		if c.GetAllowExpansion() {
			score++
		}
		if score > bestScore || (score == bestScore && c.GetName() < best) {
			best, bestScore = c.GetName(), score
		}
	}
	return best
}

// unlimit drops reasons from the LIMITED reasons found so far.
func (r *run) unlimit(reasons ...string) {
	r.limited = slices.DeleteFunc(r.limited, func(have string) bool { return slices.Contains(reasons, have) })
}

// sharePods is the deep test's two-pod probe: one RWO volume of the share's
// class mounted by two pods on one node. A pass keeps single-node mode; a
// failure puts the connection on the copy tier (shared_storage again says
// so); a probe that could not tell (the volume never bound) changes
// nothing.
func (r *run) sharePods(ctx context.Context) {
	r.startSharePods(ctx)()
}

// startSharePods starts the two-pod probe and returns what waits for it and
// reports it: the probe touches only the cluster until then, so the deep
// test's other probes run beside it.
func (r *run) startSharePods(ctx context.Context) func() {
	switch {
	case !r.nsExists:
		r.skip(CheckSharePods, "namespace missing")
		return func() {}
	case r.t.Cluster.ID == "":
		r.skip(CheckSharePods, "save the connection to probe its volumes")
		return func() {}
	}
	r.running(CheckSharePods)
	ctx, cancel := context.WithTimeout(ctx, sharePodsTimeout)
	_, class, _ := r.shareClass()
	type outcome struct {
		mounted bool
		why     string
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		mounted, why, err := r.probeSharePods(ctx, class)
		done <- outcome{mounted, why, err}
	}()
	return func() {
		defer cancel()
		o := <-done
		r.reportSharePods(ctx, o.mounted, o.why, o.err)
	}
}

// reportSharePods reports the two-pod probe's outcome.
func (r *run) reportSharePods(ctx context.Context, mounted bool, why string, err error) {
	switch {
	case err != nil:
		r.warn(CheckSharePods, "the probe could not tell: "+err.Error(), "")
		return
	case mounted:
		yes := true
		r.multiPodRWO = &yes
		r.res.MultiPodRWO = &yes
		r.pass(CheckSharePods, "two pods on one node mounted one ReadWriteOnce volume")
	default:
		no := false
		r.multiPodRWO = &no
		r.res.MultiPodRWO = &no
		r.fail(CheckSharePods, "a second pod on the node could not mount the volume the first one had: "+why, "")
	}
	if err := r.t.StorageCache.SetMultiPodRWO(*r.multiPodRWO); err != nil {
		r.warning("storage test cache: " + err.Error())
	}
	r.sharedStorage(ctx)
}

// probeSharePods mounts one small RWO volume of class from two pods on one
// node; mounted reports whether the second did (why not, when not).
// Everything it made is deleted before it returns.
func (r *run) probeSharePods(ctx context.Context, class string) (mounted bool, why string, err error) {
	ns := r.t.Cluster.Namespace
	cs := r.t.Client
	id := probeID()
	claim := "fleet-probe-rwo-" + id
	first, second := claim+"-a", claim+"-b"
	pods := cs.CoreV1().Pods(ns)
	pvcs := cs.CoreV1().PersistentVolumeClaims(ns)
	zero := int64(0)
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		_ = pods.Delete(dctx, second, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = pods.Delete(dctx, first, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = pvcs.Delete(dctx, claim, metav1.DeleteOptions{})
	}()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: ns, Labels: r.probeLabels(id + "-rwo")},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if class != "" {
		pvc.Spec.StorageClassName = &class
	}
	cctx, cancel := call(ctx)
	_, err = pvcs.Create(cctx, pvc, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return false, "", fmt.Errorf("creating the probe's volume: %s", Message(err))
	}
	mount := func(name, probe string, command []string, node string) (*corev1.Pod, error) {
		pod, err := r.probePod(name, probe, command)
		if err != nil {
			return nil, err
		}
		pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
		pod.Spec.NodeName = node
		return pod, nil
	}
	a, err := mount(first, id+"-a", []string{"sleep", "600"}, "")
	if err != nil {
		return false, "", err
	}
	cctx, cancel = call(ctx)
	_, err = pods.Create(cctx, a, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return false, "", fmt.Errorf("creating the first probe pod: %s", Message(err))
	}
	sctx, scancel := context.WithTimeout(ctx, sharePodsStart)
	node, err := r.waitRunning(sctx, first)
	scancel()
	if err != nil {
		return false, "", err
	}
	b, err := mount(second, id+"-b", []string{"ls", "/data"}, node)
	if err != nil {
		return false, "", err
	}
	b.Spec.RestartPolicy = corev1.RestartPolicyNever
	cctx, cancel = call(ctx)
	_, err = pods.Create(cctx, b, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return false, "", fmt.Errorf("creating the second probe pod: %s", Message(err))
	}
	deadline := time.Now().Add(sharePodsMount)
	last := "not started"
	for {
		gctx, cancel := call(ctx)
		got, err := pods.Get(gctx, second, metav1.GetOptions{})
		cancel()
		if err == nil {
			switch got.Status.Phase {
			case corev1.PodSucceeded, corev1.PodRunning:
				return true, "", nil
			case corev1.PodFailed:
				return false, "it failed: " + got.Status.Message, nil
			}
			if w := waitingOf(got); w != nil {
				last = w.Reason
				if w.Message != "" {
					last += ": " + w.Message
				}
			}
		}
		if time.Now().After(deadline) {
			if ev := r.mountEvent(ctx, second); ev != "" {
				last = ev
			}
			return false, fmt.Sprintf("after %s it was still %s", sharePodsMount, last), nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(sharePodsPoll):
		}
	}
}

// waitRunning waits for pod name to run and returns its node.
func (r *run) waitRunning(ctx context.Context, name string) (string, error) {
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	last := "waiting to be scheduled"
	for {
		gctx, cancel := call(ctx)
		got, err := pods.Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if got.Status.Phase == corev1.PodRunning && got.Spec.NodeName != "" {
				return got.Spec.NodeName, nil
			}
			if w := waitingOf(got); w != nil {
				last = w.Reason
			} else if u := unschedulable(got); u != "" {
				last = "unschedulable: " + u
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the first probe pod never ran (%s)", last)
		case <-time.After(sharePodsPoll):
		}
	}
}

// mountEvent is the latest FailedMount or FailedAttachVolume event of pod
// name ("" when none).
func (r *run) mountEvent(ctx context.Context, name string) string {
	cctx, cancel := call(ctx)
	defer cancel()
	evs, err := r.t.Client.CoreV1().Events(r.t.Cluster.Namespace).List(cctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + name})
	if err != nil {
		return ""
	}
	var latest *corev1.Event
	for i := range evs.Items {
		ev := &evs.Items[i]
		if ev.Reason != "FailedMount" && ev.Reason != "FailedAttachVolume" {
			continue
		}
		if latest == nil || ev.LastTimestamp.After(latest.LastTimestamp.Time) {
			latest = ev
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Reason + ": " + latest.Message
}
