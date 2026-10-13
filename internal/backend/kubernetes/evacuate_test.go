//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"k8s.io/apimachinery/pkg/watch"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

func TestNodeDisruption(t *testing.T) {
	taint := func(key, value string) *corev1.Node {
		return &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: key, Value: value, Effect: corev1.TaintEffectNoSchedule}}}}
	}
	for _, tc := range []struct {
		name string
		node *corev1.Node
		want string
	}{
		{"no node", nil, ""},
		{"a schedulable node", &corev1.Node{}, ""},
		{"cordoned", &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}, "cordoned"},
		{"the unschedulable taint", taint(corev1.TaintNodeUnschedulable, ""), "cordoned"},
		{"the autoscaler removing it", taint("ToBeDeletedByClusterAutoscaler", "1700000000"), "being removed by the cluster autoscaler"},
		{"Karpenter v1", taint("karpenter.sh/disrupted", ""), "being replaced by Karpenter"},
		{"Karpenter v1beta1", taint("karpenter.sh/disruption", "disrupting"), "being replaced by Karpenter"},
		{"Karpenter v1beta1, another value", taint("karpenter.sh/disruption", "candidate"), ""},
		{"the autoscaler's soft candidate taint", taint("DeletionCandidateOfClusterAutoscaler", "1700000000"), ""},
		{"a taint of its own", taint("dedicated", "gpu"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NodeDisruption(tc.node); got != tc.want {
				t.Fatalf("NodeDisruption = %q, want %q", got, tc.want)
			}
		})
	}
}

// holderPod is an instance pod of share group abcdefgh placed in mode m on
// node.
func holderPod(m topology.Mode, node string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{names.LabelShareGroup: "abcdefgh", names.LabelRole: string(names.RoleInstance)}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	p.Spec.Affinity = topology.Placement{Mode: m, ShareGroup: "abcdefgh"}.Affinity(true)
	return p
}

func TestSingleNodeHolder(t *testing.T) {
	if !singleNodeHolder(holderPod(topology.SingleNode, "n1")) {
		t.Fatal("an instance pod placed in single-node mode is no holder")
	}
	for name, p := range map[string]*corev1.Pod{
		"multi-node mode": holderPod(topology.MultiNode, "n1"),
		"copy tier":       holderPod(topology.Copy, "n1"),
		"no share group": func() *corev1.Pod {
			p := holderPod(topology.SingleNode, "n1")
			delete(p.Labels, names.LabelShareGroup)
			return p
		}(),
		"a Job's pod": func() *corev1.Pod {
			p := holderPod(topology.SingleNode, "n1")
			p.Labels[names.LabelRole] = string(names.RoleSeed)
			return p
		}(),
		"another group's affinity": func() *corev1.Pod {
			p := holderPod(topology.SingleNode, "n1")
			p.Labels[names.LabelShareGroup] = "zzzzzzzz"
			return p
		}(),
		"no pod": nil,
	} {
		if singleNodeHolder(p) {
			t.Errorf("%s: a holder", name)
		}
	}
	if n := holderNode(holderPod(topology.SingleNode, "n1"), true); n != "" {
		t.Fatalf("a deleted holder's node = %q", n)
	}
}

func TestPinnedTo(t *testing.T) {
	host := func(values ...string) corev1.NodeSelectorTerm {
		return corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: values}}}
	}
	zone := corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eu-1a"}}}}
	byName := corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}}}
	for _, tc := range []struct {
		name  string
		terms []corev1.NodeSelectorTerm
		want  bool
	}{
		{"no terms (any node)", nil, false},
		{"the node by hostname", []corev1.NodeSelectorTerm{host("n1")}, true},
		{"the node by name", []corev1.NodeSelectorTerm{byName}, true},
		{"the node and a zone", []corev1.NodeSelectorTerm{{MatchExpressions: append(host("n1").MatchExpressions, zone.MatchExpressions...)}}, true},
		{"another node", []corev1.NodeSelectorTerm{host("n2")}, false},
		{"the node or another", []corev1.NodeSelectorTerm{host("n1", "n2")}, false},
		{"the node, or a zone", []corev1.NodeSelectorTerm{host("n1"), zone}, false},
		{"a zone", []corev1.NodeSelectorTerm{zone}, false},
		{"not the node", []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"n1"}}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedTo(tc.terms, "n1"); got != tc.want {
				t.Fatalf("pinnedTo = %v, want %v", got, tc.want)
			}
		})
	}
}

// evacObjects are a fleet's objects on the handle rig's connection: a
// running instance per name (its StatefulSet and disk PVC), its share PVC,
// and a volume behind each claim, attached to n1.
func evacObjects(instances ...string) []kruntime.Object {
	var objs []kruntime.Object
	claim := func(name, class string) {
		objs = append(objs,
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: handleNS}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptrTo(class), VolumeName: "pv-" + name}},
			&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-" + name}},
			&storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "va-" + name}, Spec: storagev1.VolumeAttachmentSpec{NodeName: "n1", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: ptrTo("pv-" + name)}}, Status: storagev1.VolumeAttachmentStatus{Attached: true}},
		)
	}
	claim("fs-alpha-share", "block")
	for _, name := range instances {
		one := int32(1)
		objs = append(objs, &appsv1.StatefulSet{ObjectMeta: handleMeta(name), Spec: appsv1.StatefulSetSpec{Replicas: &one}})
		claim(name, "block")
	}
	return objs
}

// evacRig records what a move did to its members, in order.
type evacRig struct {
	*handleRig
	mu     sync.Mutex
	events []string
}

func newEvacRig(t *testing.T, objs ...kruntime.Object) *evacRig {
	t.Helper()
	detachWait, detachPoll, drainGap := evacuationDetachWait, evacuationDetachPoll, evacuationDrainGap
	evacuationDetachWait, evacuationDetachPoll, evacuationDrainGap = 5*time.Second, 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		evacuationDetachWait, evacuationDetachPoll, evacuationDrainGap = detachWait, detachPoll, drainGap
	})
	return &evacRig{handleRig: newHandleRig(t, objs...)}
}

func (r *evacRig) note(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *evacRig) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *evacRig) annotation(sts string) (string, bool) {
	set, err := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), sts, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	v, ok := set.Annotations[names.AnnotationMovingOff]
	return v, ok
}

// member is an instance whose stop detaches its disk (and the share, for
// the first) from n1, and whose start runs its pod on n2.
func (r *evacRig) member(name string) EvacuationMember {
	ctx := context.Background()
	return EvacuationMember{
		Instance: name, StatefulSet: name,
		Stop: func() error {
			if mark, _ := r.annotation(name); mark != "n1" {
				r.t.Errorf("%s stopped unmarked (mark %q)", name, mark)
			}
			r.note("stop " + name)
			go func() {
				time.Sleep(20 * time.Millisecond)
				_ = r.cs.StorageV1().VolumeAttachments().Delete(ctx, "va-"+name, metav1.DeleteOptions{})
				_ = r.cs.StorageV1().VolumeAttachments().Delete(ctx, "va-fs-alpha-share", metav1.DeleteOptions{})
			}()
			return nil
		},
		Start: func() error {
			list, _ := r.cs.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
			if len(list.Items) != 0 {
				r.t.Errorf("%s started while %d volumes were still attached to the node", name, len(list.Items))
			}
			r.note("start " + name)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(name), Namespace: handleNS}, Spec: corev1.PodSpec{NodeName: "n2"}}
			_, err := r.cs.CoreV1().Pods(handleNS).Create(ctx, pod, metav1.CreateOptions{})
			return err
		},
	}
}

func (r *evacRig) evacuation(members ...EvacuationMember) Evacuation {
	return Evacuation{Cluster: handleCluster, Node: "n1", Why: "cordoned", Fleet: "alpha", Share: "fs-alpha-share", Members: members}
}

// The fleet is marked, stopped as a whole, waited for to leave the node,
// started together and unmarked; the cluster's restart of each is told as a
// move.
func TestEvacuateMovesTheFleet(t *testing.T) {
	r := newEvacRig(t, evacObjects("a", "b")...)
	res, err := r.pool.Evacuate(context.Background(), r.evacuation(r.member("a"), r.member("b")))
	if err != nil || res.Blocked != "" {
		t.Fatalf("Evacuate = %+v, %v", res, err)
	}
	ev := r.seen()
	if len(ev) != 4 || !strings.HasPrefix(ev[0], "stop") || !strings.HasPrefix(ev[1], "stop") || !strings.HasPrefix(ev[2], "start") || !strings.HasPrefix(ev[3], "start") {
		t.Fatalf("events %v, want both stopped before either starts", ev)
	}
	for _, o := range res.Outcomes {
		if !o.Stopped || !o.Started || o.Skipped || o.Err != nil {
			t.Fatalf("outcome %+v", o)
		}
	}
	if res.To != "n2" {
		t.Fatalf("moved to %q, want n2", res.To)
	}
	for _, sts := range []string{"a", "b"} {
		if v, ok := r.annotation(sts); ok {
			t.Fatalf("%s still marked moving off %q", sts, v)
		}
		r.pool.instMu.Lock()
		e := r.pool.tracks[trackKey{handleCluster, sts}].expected
		r.pool.instMu.Unlock()
		if e == nil || e.cause != backend.RestartFleetMoved {
			t.Fatalf("%s: expected replacement %+v, want a fleet move", sts, e)
		}
	}
}

// An instance protected from drains stays stopped past kubectl drain's
// eviction retry, so the drain finds its pod gone rather than asking to evict
// the new one (same name, refused by the same budget); without one the fleet
// starts again as soon as it can.
func TestEvacuateOutwaitsADrainsRetry(t *testing.T) {
	for _, protected := range []bool{false, true} {
		objs := evacObjects("a", "b")
		if protected {
			objs = append(objs, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: handleNS}})
		}
		r := newEvacRig(t, objs...)
		var mu sync.Mutex
		var stopped, started time.Time
		a, b := r.member("a"), r.member("b")
		for _, m := range []*EvacuationMember{&a, &b} {
			stop, start := m.Stop, m.Start
			m.Stop = func() error {
				err := stop()
				mu.Lock()
				stopped = time.Now()
				mu.Unlock()
				return err
			}
			m.Start = func() error {
				mu.Lock()
				if started.IsZero() {
					started = time.Now()
				}
				mu.Unlock()
				return start()
			}
		}
		if _, err := r.pool.Evacuate(context.Background(), r.evacuation(a, b)); err != nil {
			t.Fatal(err)
		}
		gap := started.Sub(stopped)
		if protected && gap < evacuationDrainGap {
			t.Fatalf("a protected fleet started %v after its stop, want at least %v", gap, evacuationDrainGap)
		}
		if !protected && gap >= evacuationDrainGap {
			t.Fatalf("an unprotected fleet waited %v to start again", gap)
		}
	}
}

// A member stopped or destroyed meanwhile is left stopped; one that does
// not start again is reported, and every mark goes all the same.
func TestEvacuateOutcomes(t *testing.T) {
	r := newEvacRig(t, evacObjects("a", "b", "c")...)
	a, b, c := r.member("a"), r.member("b"), r.member("c")
	b.Wanted = func() bool { return false }
	c.Start = func() error { return errors.New("pod did not come up") }
	res, err := r.pool.Evacuate(context.Background(), r.evacuation(a, b, c))
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Outcomes[0]; !o.Started {
		t.Fatalf("a: %+v", o)
	}
	if o := res.Outcomes[1]; !o.Stopped || !o.Skipped || o.Started || o.Err != nil {
		t.Fatalf("b (stopped meanwhile): %+v", o)
	}
	if o := res.Outcomes[2]; !o.Stopped || o.Started || o.Err == nil || !strings.Contains(o.Err.Error(), "starting it again: pod did not come up") {
		t.Fatalf("c (did not start): %+v", o)
	}
	if slices.Contains(r.seen(), "start b") {
		t.Fatal("a member stopped meanwhile was started")
	}
	r.pool.instMu.Lock()
	tb := r.pool.tracks[trackKey{handleCluster, "b"}]
	r.pool.instMu.Unlock()
	if tb != nil && tb.expected != nil {
		t.Fatal("a member left stopped expects a replacement")
	}
	for _, sts := range []string{"a", "b", "c"} {
		if _, ok := r.annotation(sts); ok {
			t.Fatalf("%s still marked", sts)
		}
	}

	// One that does not stop is not started.
	r2 := newEvacRig(t, evacObjects("d")...)
	d := r2.member("d")
	d.Stop = func() error { return errors.New("pod still terminating") }
	res, err = r2.pool.Evacuate(context.Background(), r2.evacuation(d))
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Outcomes[0]; o.Stopped || o.Started || o.Err == nil || !strings.Contains(o.Err.Error(), "stopping it") {
		t.Fatalf("d (did not stop): %+v", o)
	}
}

// A daemon that stops mid-move starts nothing: the marks stay for the next
// one, which finishes the move.
func TestEvacuateStoppedMidMoveLeavesItsMarks(t *testing.T) {
	r := newEvacRig(t, evacObjects("a")...)
	ctx, cancel := context.WithCancel(context.Background())
	a := r.member("a")
	stop := a.Stop
	a.Stop = func() error {
		err := stop()
		cancel()
		return err
	}
	res, err := r.pool.Evacuate(ctx, r.evacuation(a))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Evacuate = %+v, %v; want the context's end", res, err)
	}
	if slices.Contains(r.seen(), "start a") {
		t.Fatal("started during the daemon's shutdown")
	}
	if mark, ok := r.annotation("a"); !ok || mark != "n1" {
		t.Fatalf("mark %q (%v), want it kept for the next daemon", mark, ok)
	}
}

// A mark that outlived its move is removed; an instance gone meanwhile is no
// failure.
func TestClearMovingOff(t *testing.T) {
	objs := evacObjects("a")
	for _, o := range objs {
		if set, ok := o.(*appsv1.StatefulSet); ok {
			set.Annotations = map[string]string{names.AnnotationMovingOff: "n1", "keep": "me"}
		}
	}
	r := newEvacRig(t, objs...)
	if err := r.pool.ClearMovingOff(context.Background(), handleCluster, "a"); err != nil {
		t.Fatal(err)
	}
	if v, ok := r.annotation("a"); ok {
		t.Fatalf("still marked moving off %q", v)
	}
	set, err := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil || set.Annotations["keep"] != "me" {
		t.Fatalf("the other annotations: %v, %v", set.GetAnnotations(), err)
	}
	if err := r.pool.ClearMovingOff(context.Background(), handleCluster, "gone"); err != nil {
		t.Fatalf("an instance gone meanwhile: %v", err)
	}
}

// A fleet whose volumes (or pods) cannot leave the node is not touched.
func TestEvacuateLeavesAPinnedFleet(t *testing.T) {
	onN1 := &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}},
	}}}}
	inZone := &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eu-1a"}}},
	}}}}
	withPV := func(name string, a *corev1.VolumeNodeAffinity) func([]kruntime.Object) {
		return func(objs []kruntime.Object) {
			for _, o := range objs {
				if pv, ok := o.(*corev1.PersistentVolume); ok && pv.Name == name {
					pv.Spec.NodeAffinity = a
				}
			}
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func([]kruntime.Object)
		react  func(*evacRig)
		facts  bool
		want   string
	}{
		{name: "a node-local share", mutate: withPV("pv-fs-alpha-share", onN1), want: "its share is node-local, class block"},
		{name: "a node-local disk", mutate: withPV("pv-a", onN1), want: "a's disk is node-local, class block"},
		{name: "a zonal disk moves", mutate: withPV("pv-a", inZone), want: ""},
		{name: "pods pinned by name", mutate: func(objs []kruntime.Object) {
			for _, o := range objs {
				if set, ok := o.(*appsv1.StatefulSet); ok {
					set.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: onN1.Required}}
				}
			}
		}, want: "its pods are pinned to the node"},
		{name: "unreadable volumes of a node-local class", facts: true, react: func(r *evacRig) {
			prependReactor(r.cs, "get", "persistentvolumes", func(k8stesting.Action) (bool, kruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("cluster-scoped"))
			})
		}, want: "its share is node-local, class block"},
		// A node-local class's volume may name its node by the driver's own
		// key (csi-hostpath, TopoLVM), or not at all (minikube's hostpath).
		{name: "a node-local class's volume pinned by the driver's key", facts: true, mutate: withPV("pv-fs-alpha-share", &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.hostpath.csi/node", Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}},
		}}}}), want: "its share is node-local, class block"},
		{name: "a node-local class's volume naming no node", facts: true, want: "its share is node-local, class block"},
		{name: "unreadable volumes of a class preflight does not know", react: func(r *evacRig) {
			prependReactor(r.cs, "get", "persistentvolumes", func(k8stesting.Action) (bool, kruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("cluster-scoped"))
			})
		}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := evacObjects("a")
			if tc.mutate != nil {
				tc.mutate(objs)
			}
			r := newEvacRig(t, objs...)
			if tc.react != nil {
				tc.react(r)
			}
			if tc.facts {
				SetEnvironment(Environment{Facts: func(context.Context, string) (Facts, error) {
					return Facts{Capabilities: &fleetgrpc.KubeCapabilities{StorageClasses: []*fleetgrpc.KubeStorageClass{{Name: "block", NodeLocal: true}}}}, nil
				}})
				t.Cleanup(func() { SetEnvironment(Environment{}) })
			}
			res, err := r.pool.Evacuate(context.Background(), r.evacuation(r.member("a")))
			if err != nil {
				t.Fatal(err)
			}
			if res.Blocked != tc.want {
				t.Fatalf("Blocked = %q, want %q", res.Blocked, tc.want)
			}
			if tc.want != "" {
				if ev := r.seen(); len(ev) != 0 {
					t.Fatalf("a pinned fleet was touched: %v", ev)
				}
				if _, ok := r.annotation("a"); ok {
					t.Fatal("a pinned fleet was marked")
				}
			}
		})
	}
}

func TestWaitDetached(t *testing.T) {
	ctx := context.Background()
	claims := []string{"fs-alpha-share", "a"}

	// A volume attached to another node is no wait.
	r := newEvacRig(t, evacObjects("a")...)
	if err := r.cs.StorageV1().VolumeAttachments().Delete(ctx, "va-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cs.StorageV1().VolumeAttachments().Update(ctx, &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "va-fs-alpha-share"}, Spec: storagev1.VolumeAttachmentSpec{NodeName: "n2", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: ptrTo("pv-fs-alpha-share")}}}, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.waitDetached(ctx, r.cs, handleNS, "n1", claims); err != nil {
		t.Fatalf("volumes on another node: %v", err)
	}

	// Still attached: the wait ends with an error.
	evacuationDetachWait = 50 * time.Millisecond
	r = newEvacRig(t, evacObjects("a")...)
	evacuationDetachWait = 50 * time.Millisecond
	if err := r.pool.waitDetached(ctx, r.cs, handleNS, "n1", claims); err == nil || !strings.Contains(err.Error(), "still attached to node n1") {
		t.Fatalf("attached volumes: %v", err)
	}

	// VolumeAttachments not readable: no wait.
	r = newEvacRig(t, evacObjects("a")...)
	prependReactor(r.cs, "list", "volumeattachments", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattachments"}, "", errors.New("cluster-scoped"))
	})
	if err := r.pool.waitDetached(ctx, r.cs, handleNS, "n1", claims); err != nil {
		t.Fatalf("unreadable attachments: %v", err)
	}
}

// The pool watches the nodes its single-node holders are on, and reports a
// node the cluster empties on the observation of each instance there, with
// a move begun on it.
func TestNodeWatchesFollowTheHolders(t *testing.T) {
	ctx := context.Background()
	n1, n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}
	one := int32(1)
	set := &appsv1.StatefulSet{ObjectMeta: handleMeta(handleSTS()), Spec: appsv1.StatefulSetSpec{Replicas: &one}}
	r := newHandleRig(t, n1, n2, set)
	changes := make(chan string, 100)
	r.pool.OnChange(func(_, sts string) { changes <- sts })
	// A node watch is a list and then a watch, and the fake clientset drops
	// what changes between the two: the test cordons a node only once its
	// watch is there.
	nodeWatches := make(chan struct{}, 8)
	r.cs.PrependWatchReactor("nodes", func(a k8stesting.Action) (bool, watch.Interface, error) {
		var opts metav1.ListOptions
		if wa, ok := a.(k8stesting.WatchActionImpl); ok {
			opts = wa.ListOptions
		}
		w, err := r.cs.Tracker().Watch(a.GetResource(), a.GetNamespace(), opts)
		nodeWatches <- struct{}{}
		return true, w, err
	})
	r.pool.Track([]Tracked{{Cluster: handleCluster, StatefulSet: handleSTS()}})

	pod := holderPod(topology.SingleNode, "n1")
	pod.ObjectMeta = handleMeta(names.Pod(handleSTS()))
	pod.Labels[names.LabelShareGroup] = "abcdefgh"
	if _, err := r.cs.CoreV1().Pods(handleNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	evacSoon(t, "the holder on n1, its node watched", func() bool {
		o := r.pool.Observe(handleCluster, handleSTS())
		return o.Holder && o.Node == "n1" && r.watchedNodes() == "n1"
	})
	if o := r.pool.Observe(handleCluster, handleSTS()); o.Disrupted != "" || o.MovingOff != "" {
		t.Fatalf("before the cordon: %+v", o)
	}

	select {
	case <-nodeWatches:
	case <-time.After(10 * time.Second):
		t.Fatal("n1's watch never started")
	}
	for len(changes) > 0 {
		<-changes
	}
	n1.Spec.Unschedulable = true
	if _, err := r.cs.CoreV1().Nodes().Update(ctx, n1, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	evacSoon(t, "the cordon seen", func() bool { return r.pool.Observe(handleCluster, handleSTS()).Disrupted == "cordoned" })
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("the cordon was not told")
	}

	set.Annotations = map[string]string{names.AnnotationMovingOff: "n1"}
	if _, err := r.cs.AppsV1().StatefulSets(handleNS).Update(ctx, set, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	evacSoon(t, "the move's mark", func() bool { return r.pool.Observe(handleCluster, handleSTS()).MovingOff == "n1" })

	// The pod comes back on n2: n1 is no longer watched, n2 is.
	if err := r.cs.CoreV1().Pods(handleNS).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := pod.DeepCopy()
	moved.UID, moved.ResourceVersion, moved.Spec.NodeName = types.UID("uid-moved"), "", "n2"
	if _, err := r.cs.CoreV1().Pods(handleNS).Create(ctx, moved, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	evacSoon(t, "the holder on n2, its node watched", func() bool {
		o := r.pool.Observe(handleCluster, handleSTS())
		return o.Node == "n2" && o.Disrupted == "" && r.watchedNodes() == "n2"
	})

	// A pod that holds no single-node share has no node watched.
	plain := moved.DeepCopy()
	plain.Spec.Affinity = nil
	plain.ResourceVersion = ""
	if _, err := r.cs.CoreV1().Pods(handleNS).Update(ctx, plain, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	evacSoon(t, "no node watched", func() bool { return r.watchedNodes() == "" })
}

// evacSoon waits for cond, failing with what after 10s.
func evacSoon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// watchedNodes are the rig connection's watched nodes, comma-joined.
func (r *handleRig) watchedNodes() string {
	r.pool.nodeMu.Lock()
	defer r.pool.nodeMu.Unlock()
	var out []string
	if ws := r.pool.nodes[handleCluster]; ws != nil {
		for n := range ws.byNode {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}
