//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

const shareDaemon = "0123456789abcdef0123456789abcdef"

// shareLayout is the resolver's layout of a fleet with Claude Code's mounts.
var shareLayout = []backend.LayoutEntry{
	{Path: ".claude", Dir: true, Mode: 0o777},
	{Path: "files", Dir: true, Mode: 0o777},
	{Path: "files/.claude.json", Mode: 0o666, Seed: "{}"},
}

// wffc and immediate are StorageClasses as the preflight classifies them.
var (
	wffc      = &fleetgrpc.KubeStorageClass{Name: "standard", IsDefault: true, Block: true, NodeLocal: true, BindingMode: string(storagev1.VolumeBindingWaitForFirstConsumer), AllowExpansion: true}
	immediate = &fleetgrpc.KubeStorageClass{Name: "fast", Block: true, BindingMode: string(storagev1.VolumeBindingImmediate)}
)

// shareCreation is a create of instance name in fleet alpha with Claude
// Code's mounts on, connected to cs (as connect leaves it).
func shareCreation(cs *fake.Clientset, name string, caps *fleetgrpc.KubeCapabilities, rec fleet.KubeCluster) *creation {
	key := names.InstanceKey(shareDaemon, "alpha", name, time.Unix(1700000000, 0))
	rec.ID, rec.Namespace = handleCluster, handleNS
	return &creation{
		id: bound{cluster: handleCluster, key: key, fleet: "alpha", name: name, sts: names.Instance("alpha", name, key)},
		cs: cs, ns: handleNS, rec: rec, daemonID: shareDaemon, owner: handleOwner,
		facts: Facts{Capabilities: caps},
		req:   backend.UpRequest{Layout: shareLayout},
	}
}

func shareCaps(classes ...*fleetgrpc.KubeStorageClass) *fleetgrpc.KubeCapabilities {
	return &fleetgrpc.KubeCapabilities{Version: "v1.37.0", StorageClasses: classes, Nodes: []*fleetgrpc.KubeNode{{Name: "node-a"}, {Name: "node-b"}}}
}

func h8() string { return names.FleetHash(shareDaemon, handleCluster, "alpha") }

// bindShare binds the fleet's share to a volume on node, as a scheduler
// would once a pod mounting it lands there.
func bindShare(t *testing.T, cs *fake.Clientset, node string) {
	t.Helper()
	ctx := context.Background()
	claim := ShareClaim("alpha", h8())
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-share"}, Spec: corev1.PersistentVolumeSpec{
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.hostpath.csi/node", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
		}}}},
	}}
	if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = pv.Name
	pvc.Status.Phase = corev1.ClaimBound
	if _, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareShareBindsTheFleetsShareOnce: the fleet's first instance makes
// its share (single-node mode, the instance disks' class, the connection's
// size) and its seed binds it beside its own disk, holding the fleet's bind
// lock; a second create of the fleet waits for that, then takes the share's
// recorded topology without binding anything.
func TestPrepareShareBindsTheFleetsShareOnce(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	first := shareCreation(cs, "web", shareCaps(wffc), fleet.KubeCluster{ShareSize: "12Gi"})
	if err := first.prepareShare(ctx); err != nil {
		t.Fatalf("prepareShare: %v", err)
	}
	claim := ShareClaim("alpha", h8())
	pvc, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no share made: %v", err)
	}
	if size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "12Gi" || pvc.Spec.StorageClassName != nil ||
		!reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) ||
		pvc.Annotations[names.AnnotationShareMode] != string(topology.SingleNode) || pvc.Labels[names.LabelRole] != string(names.RoleShare) ||
		pvc.Labels[names.LabelFleet] != h8() || pvc.Labels[names.LabelShareGroup] != h8() || pvc.Labels[names.LabelKey] != "" {
		t.Fatalf("the share is %+v", pvc)
	}
	s := first.share
	if !s.bind || len(first.seedJob(ctx, "", false).Claims) != 1 || first.seedJob(ctx, "", false).Claims[0].ClaimName != claim {
		t.Fatalf("the first instance's seed does not bind the share: bind %v claims %+v", s.bind, first.seedJob(ctx, "", false).Claims)
	}
	if in := s.input(); in.Claim != claim || in.Layout.UID < 0 || len(in.Layout.Entries) != 3 || in.Layout.Entries[2].Seed != "{}" {
		t.Fatalf("the render's share is %+v", in)
	}

	second := shareCreation(cs, "api", shareCaps(wffc), fleet.KubeCluster{})
	done := make(chan error, 1)
	go func() { done <- second.prepareShare(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("the second create did not wait for the share to bind: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	first.share.seedNode = "node-a"
	bindShare(t, cs, "node-a")
	if err := first.finishShare(ctx, true); err != nil {
		t.Fatalf("finishShare: %v", err)
	}
	want := []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.hostpath.csi/node", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}}}}}
	if !reflect.DeepEqual(first.share.placement.Terms, want) {
		t.Fatalf("the first create placed the fleet by %+v, want the volume's topology", first.share.placement.Terms)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second create: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second create still waits once the share is bound")
	}
	if second.share.bind || !reflect.DeepEqual(second.share.placement.Terms, want) {
		t.Fatalf("the second create: bind %v, placed by %+v", second.share.bind, second.share.placement.Terms)
	}
	pvc, _ = cs.CoreV1().PersistentVolumeClaims(handleNS).Get(ctx, claim, metav1.GetOptions{})
	var recorded []corev1.NodeSelectorTerm
	if err := json.Unmarshal([]byte(pvc.Annotations[names.AnnotationShareTopology]), &recorded); err != nil || !reflect.DeepEqual(recorded, want) {
		t.Fatalf("the share's recorded topology is %q (%v)", pvc.Annotations[names.AnnotationShareTopology], err)
	}
	p := second.share.placementFor()
	if a := p.Affinity(true); a == nil || a.PodAffinity == nil || a.NodeAffinity == nil {
		t.Fatalf("an instance pod's affinity %+v", a)
	}
	rec := second.share.record(&podspec.PodSpec{})
	if rec.Mode != "single-node" || rec.Claim != claim || len(rec.Roots) != 2 || rec.Roots[1].Name != "files" || !reflect.DeepEqual(rec.Roots[1].Reserved, []string{".claude.json"}) {
		t.Fatalf("the instance record's share %+v", rec)
	}
}

// TestPrepareShareWithoutReadingVolumes: a connection that may not read
// PersistentVolumes (cluster-scoped) places the fleet on the node its
// binding seed ran on.
func TestPrepareShareWithoutReadingVolumes(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "persistentvolumes", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv-share", nil)
	})
	ctx := context.Background()
	c := shareCreation(cs, "web", shareCaps(wffc), fleet.KubeCluster{})
	if err := c.prepareShare(ctx); err != nil {
		t.Fatal(err)
	}
	bindShare(t, cs, "node-b")
	c.share.seedNode = "node-b"
	if err := c.finishShare(ctx, true); err != nil {
		t.Fatal(err)
	}
	want := []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"node-b"}}}}}
	if !reflect.DeepEqual(c.share.placement.Terms, want) {
		t.Fatalf("placed by %+v, want the seed's node", c.share.placement.Terms)
	}
}

// TestPrepareShareOnTheCopyTier: where two pods on one node cannot mount one
// RWO volume, the fleet has no share: each instance's copy is its own.
func TestPrepareShareOnTheCopyTier(t *testing.T) {
	cs := fake.NewClientset()
	caps := shareCaps(wffc)
	caps.ShareMode = string(topology.Copy)
	c := shareCreation(cs, "web", caps, fleet.KubeCluster{})
	if err := c.prepareShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l, _ := cs.CoreV1().PersistentVolumeClaims(handleNS).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatalf("a share was made on the copy tier: %v", l.Items)
	}
	if in := c.share.input(); in.Claim != "" || c.share.placementFor() != nil || c.share.bind {
		t.Fatalf("copy tier: input %+v placement %+v bind %v", in, c.share.placementFor(), c.share.bind)
	}
	if rec := c.share.record(&podspec.PodSpec{}); rec.Mode != "copy" || rec.Claim != "" {
		t.Fatalf("copy tier record %+v", rec)
	}
}

// TestPrepareShareRefusesImmediateBindingOnManyNodes: a class that binds
// each volume when made would put the share and the instance disks on
// different nodes; the create is refused with the fix, making nothing.
func TestPrepareShareRefusesImmediateBindingOnManyNodes(t *testing.T) {
	cs := fake.NewClientset()
	c := shareCreation(cs, "web", shareCaps(wffc, immediate), fleet.KubeCluster{SharedStorageClass: "fast"})
	err := c.prepareShare(context.Background())
	code, _ := kubeerr.Of(err)
	if code != kubeerr.Misconfigured.Narrow("share_binding") || !strings.Contains(err.Error(), "WaitForFirstConsumer") || !strings.Contains(err.Error(), "storage class fast") {
		t.Fatalf("prepareShare = %v (%s), want the Immediate class refused with its fix", err, code)
	}
	if l, _ := cs.CoreV1().PersistentVolumeClaims(handleNS).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatal("a refused share was made")
	}
	// On a cluster of one node (minikube's Immediate hostpath class) it is
	// fine: the create waits for the share to bind, then places by it.
	one := shareCaps(wffc, immediate)
	one.Nodes = one.Nodes[:1]
	ocs := fake.NewClientset()
	single := shareCreation(ocs, "web", one, fleet.KubeCluster{SharedStorageClass: "fast"})
	go func() {
		for {
			if _, err := ocs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), ShareClaim("alpha", h8()), metav1.GetOptions{}); err == nil {
				bindShare(t, ocs, "minikube")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := single.prepareShare(context.Background()); err != nil {
		t.Fatalf("an Immediate class on a one-node cluster (minikube's hostpath): %v", err)
	}
	if single.share.bind || len(single.share.placement.Terms) != 1 {
		t.Fatalf("an Immediately bound share: bind %v, placed by %+v", single.share.bind, single.share.placement.Terms)
	}
	// A multi-node share is mounted from any node: its class may bind at
	// once.
	multi := shareCaps(wffc, immediate)
	multi.ShareMode, multi.SharedStorageClass = string(topology.MultiNode), "fast"
	mcs := fake.NewClientset()
	go func() {
		for {
			if _, err := mcs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), ShareClaim("alpha", h8()), metav1.GetOptions{}); err == nil {
				bindShare(t, mcs, "node-b")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := shareCreation(mcs, "web", multi, fleet.KubeCluster{SharedStorageClass: "fast"}).prepareShare(context.Background()); err != nil {
		t.Fatalf("an Immediate class for a multi-node share: %v", err)
	}
}

// TestPrepareShareRefusesAForeignClaim: a PVC of the share's name that is not
// this daemon's is never used.
func TestPrepareShareRefusesAForeignClaim(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: ShareClaim("alpha", h8()), Namespace: handleNS, Labels: map[string]string{names.LabelManagedBy: "someone"}}}
	c := shareCreation(fake.NewClientset(foreign), "web", shareCaps(wffc), fleet.KubeCluster{})
	if err := c.prepareShare(context.Background()); err == nil || !strings.Contains(err.Error(), "not this daemon's") {
		t.Fatalf("prepareShare = %v, want the foreign PVC refused", err)
	}
}

// TestPrepareShareKeepsAnExistingSharesMode: a fleet's instances are placed
// alike, by the mode its share was made in, whatever the connection's
// preflight says now.
func TestPrepareShareKeepsAnExistingSharesMode(t *testing.T) {
	cs := fake.NewClientset()
	c := shareCreation(cs, "web", shareCaps(wffc), fleet.KubeCluster{})
	if err := c.prepareShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.share.done()
	bindShare(t, cs, "node-a")
	caps := shareCaps(wffc)
	caps.ShareMode = string(topology.Copy)
	later := shareCreation(cs, "api", caps, fleet.KubeCluster{})
	if err := later.prepareShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if later.share.mode != topology.SingleNode || later.share.input().Claim == "" {
		t.Fatalf("a later instance's share mode is %s, want the share's own single-node", later.share.mode)
	}
}

func TestLayoutOf(t *testing.T) {
	if got := layoutOf(backend.UpRequest{Layout: shareLayout}); !reflect.DeepEqual(got, shareLayout) {
		t.Fatalf("layoutOf with a layout = %+v", got)
	}
	got := layoutOf(backend.UpRequest{Mounts: []backend.Mount{
		{ContainerPath: "/h/.claude", Scope: backend.MountScopeFleetShared, SubPath: ".claude"},
		{ContainerPath: "/control", LocalPath: "/x"},
	}})
	if !reflect.DeepEqual(got, []backend.LayoutEntry{{Path: ".claude", Dir: true, Mode: 0o777}}) {
		t.Fatalf("layoutOf from mounts = %+v", got)
	}
	if layoutOf(backend.UpRequest{}) != nil {
		t.Fatal("a request without fleet mounts has a layout")
	}
}

// TestShareModeOf: a new share takes the preflight's mode, multi-node only
// in the class that passed the multi-node test (one set since is not known
// to).
func TestShareModeOf(t *testing.T) {
	for mode, want := range map[string]topology.Mode{"": topology.SingleNode, "single-node": topology.SingleNode, "copy": topology.Copy, "multi-node": topology.MultiNode, "bogus": topology.SingleNode} {
		if got := shareModeOf(&fleetgrpc.KubeCapabilities{ShareMode: mode, SharedStorageClass: "nfs"}, "nfs"); got != want {
			t.Errorf("shareModeOf(%q) = %s, want %s", mode, got, want)
		}
	}
	if got := shareModeOf(&fleetgrpc.KubeCapabilities{ShareMode: "multi-node", SharedStorageClass: "nfs"}, "cephfs"); got != topology.SingleNode {
		t.Errorf("a class the multi-node test did not pass is %s", got)
	}
	if shareModeOf(nil, "") != topology.SingleNode {
		t.Error("no preflight yet: not single-node")
	}
}
