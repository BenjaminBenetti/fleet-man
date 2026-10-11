//go:build !nok8s

package kubernetes

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// volumeRig is a connection with fleet alpha's share and the records of its
// instances, and the named volumes removed from the share (removes answers
// whether a pod of the share was there to remove them).
type volumeRig struct {
	*handleRig
	claim, h8 string
	removed   [][]string
	removes   bool
}

func newVolumeRig(t *testing.T) *volumeRig {
	t.Helper()
	r := &volumeRig{handleRig: newHandleRig(t), removes: true}
	ident, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r.h8 = names.FleetHash(ident.DaemonID, handleCluster, "alpha")
	r.claim = ShareClaim("alpha", r.h8)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: r.claim, Namespace: handleNS, Labels: shareLabels(ident.Owner(), handleCluster, r.h8)}}
	if _, err := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Create(context.Background(), pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	prev := volumeRemover
	volumeRemover = func(_ *Pool, _ context.Context, cluster, h8, claim string, ids []string) bool {
		if cluster != handleCluster || h8 != r.h8 || claim != r.claim {
			t.Errorf("removing from %s/%s (%s), not the fleet's share", cluster, claim, h8)
		}
		if r.removes {
			r.removed = append(r.removed, slices.Clone(ids))
		}
		return r.removes
	}
	t.Cleanup(func() { volumeRemover = prev })
	return r
}

// record saves and returns the record of instance name mounting volumes.
func (r *volumeRig) record(name string, volumes ...string) InstanceRecord {
	r.t.Helper()
	key := names.InstanceKey(shareDaemon, "alpha", name, time.Unix(1700000000, 0))
	rec := InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: key, Fleet: "alpha", Instance: name,
		StatefulSet: names.Instance("alpha", name, key), Gen: 1, Share: &ShareRecord{Mode: "single-node", Claim: r.claim, Volumes: volumes}}
	if err := SaveInstanceRecord(rec); err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// unused is the share's volumes marked unused.
func (r *volumeRig) unused() []string {
	r.t.Helper()
	pvc, err := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), r.claim, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return unusedVolumes(pvc)
}

// TestRetireVolumesTakesOnlyTheLastUsersVolumes: a destroyed instance takes
// the fleet's named volumes no other instance mounts with it, and leaves the
// rest; with no pod of the share to remove them, they stay marked unused for
// the upkeep, which removes them once one runs.
func TestRetireVolumesTakesOnlyTheLastUsersVolumes(t *testing.T) {
	r := newVolumeRig(t)
	ctx := context.Background()
	web := r.record("web", "0000000a", "0000000b")
	r.record("api", "0000000b")

	r.pool.RetireVolumes(ctx, web)
	if !reflect.DeepEqual(r.removed, [][]string{{"0000000a"}}) {
		t.Fatalf("removed %v, want only the volume no other instance mounts", r.removed)
	}
	if got := r.unused(); len(got) != 0 {
		t.Errorf("marks left after a removal: %v", got)
	}

	// No pod of the share runs: the volume stays marked.
	r.removed, r.removes = nil, false
	r.pool.RetireVolumes(ctx, web)
	if got := r.unused(); !slices.Equal(got, []string{"0000000a"}) {
		t.Fatalf("marked %v, want the destroyed instance's own volume", got)
	}
	// The upkeep removes it once a pod runs, and drops the mark.
	r.pool.sweepVolumes(ctx, handleCluster, r.h8, r.claim, "")
	if got := r.unused(); !slices.Equal(got, []string{"0000000a"}) {
		t.Fatalf("a sweep with no pod took the mark off: %v", got)
	}
	// The destroyed instance's record goes with its teardown.
	if err := RemoveInstanceRecord(handleCluster, web.Key); err != nil {
		t.Fatal(err)
	}
	r.removes = true
	if _, _, err := r.pool.MaintainShare(ctx, handleCluster, "alpha"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.removed, [][]string{{"0000000a"}}) || len(r.unused()) != 0 {
		t.Errorf("upkeep: removed %v, marks %v", r.removed, r.unused())
	}

	// A copy-tier instance's volumes are on its own disk.
	r.removed = nil
	copyTier := web
	copyTier.Share = &ShareRecord{Mode: "copy", Volumes: []string{"0000000c"}}
	r.pool.RetireVolumes(ctx, copyTier)
	if len(r.removed) != 0 || len(r.unused()) != 0 {
		t.Errorf("a copy-tier instance's volumes touched the share: %v %v", r.removed, r.unused())
	}
}

// TestNamedVolumesACreateClaims: a create in flight claims the volumes its
// seed fills before its record names them, so a destroy meanwhile keeps
// them; a volume a destroy left marked unused is the create's fresh, its
// mark taken off; a mark mounted again goes at the next sweep, its volume
// kept.
func TestNamedVolumesACreateClaims(t *testing.T) {
	r := newVolumeRig(t)
	ctx := context.Background()
	web := r.record("web", "0000000a", "0000000b")

	_, release := claimVolumes(ctx, r.cs, handleNS, handleCluster, r.claim, []string{"0000000a"})
	r.removes = false
	r.pool.RetireVolumes(ctx, web)
	if got := r.unused(); !slices.Equal(got, []string{"0000000b"}) {
		t.Fatalf("marked %v, want only the volume no create claims", got)
	}
	release()
	release() // once only
	if err := RemoveInstanceRecord(handleCluster, web.Key); err != nil {
		t.Fatal(err)
	}

	fresh, release := claimVolumes(ctx, r.cs, handleNS, handleCluster, r.claim, []string{"0000000b", "0000000d"})
	defer release()
	if !fresh["0000000b"] || fresh["0000000d"] || len(r.unused()) != 0 {
		t.Errorf("fresh %v, marks %v: want the destroyed volume fresh and its mark off", fresh, r.unused())
	}

	// A marked volume an instance still mounts (a record the teardown has
	// not removed yet) is kept, and keeps its mark; a create that mounts it
	// takes the mark off, but never takes it fresh.
	r.record("api", "0000000e")
	if err := markUnused(ctx, r.cs, handleNS, r.claim, []string{"0000000e"}); err != nil {
		t.Fatal(err)
	}
	r.removes = true
	r.pool.sweepVolumes(ctx, handleCluster, r.h8, r.claim, "")
	if len(r.removed) != 0 || !slices.Equal(r.unused(), []string{"0000000e"}) {
		t.Errorf("a volume something mounts: removed %v, marks %v", r.removed, r.unused())
	}
	fresh, release2 := claimVolumes(ctx, r.cs, handleNS, handleCluster, r.claim, []string{"0000000e"})
	defer release2()
	if fresh["0000000e"] || len(r.unused()) != 0 {
		t.Errorf("a volume another instance mounts: fresh %v, marks %v", fresh, r.unused())
	}
}

// TestSeedFillsTheFleetsNamedVolumes: a seed that fills the fleet's named
// volumes mounts the share (never on the copy tier, whose tree is on the
// disk) and finds it where seedPath says; its create claims the volumes,
// taking one a destroy left unused fresh.
func TestSeedFillsTheFleetsNamedVolumes(t *testing.T) {
	r := newVolumeRig(t)
	ctx := context.Background()
	c := shareCreation(r.cs, "web", shareCaps(wffc), fleet.KubeCluster{})
	c.share = &shareState{mode: topology.SingleNode, claim: r.claim}
	if cl := c.share.seedClaims(false); len(cl) != 0 {
		t.Errorf("a seed with nothing to do on a bound share mounts it: %+v", cl)
	}
	if cl := c.share.seedClaims(true); len(cl) != 1 || cl[0].ClaimName != r.claim || c.share.seedPath() != shareSeedPath {
		t.Errorf("a seed filling named volumes: claims %+v, path %s", cl, c.share.seedPath())
	}
	if err := markUnused(ctx, r.cs, handleNS, r.claim, []string{"0000000a"}); err != nil {
		t.Fatal(err)
	}
	spec := &podseed.SeedSpec{SharedVolumes: []podseed.Volume{{Name: "0000000a", Target: "/a"}, {Name: "0000000b", Target: "/b"}}}
	c.claimSharedVolumes(ctx, spec)
	if !spec.SharedVolumes[0].Fresh || spec.SharedVolumes[1].Fresh || c.volumes == nil {
		t.Errorf("claimed %+v", spec.SharedVolumes)
	}
	c.releaseVolumes()

	cp := shareCreation(r.cs, "api", shareCaps(wffc), fleet.KubeCluster{})
	cp.share = &shareState{mode: topology.Copy, claim: r.claim}
	spec = &podseed.SeedSpec{SharedVolumes: []podseed.Volume{{Name: "0000000a", Target: "/a"}}}
	cp.claimSharedVolumes(ctx, spec)
	if len(cp.share.seedClaims(true)) != 0 || cp.share.seedPath() != podspec.JobDiskPath+"/"+podseed.DirSync || cp.volumes != nil {
		t.Errorf("copy tier: claims %+v, path %s, claimed %v", cp.share.seedClaims(true), cp.share.seedPath(), cp.volumes != nil)
	}
}

// TestShareRecordKeepsTheHostBinds: the record of a pod with host binds and
// named volumes names each bound path's copy as its own synced root (a
// bound file's holding only its name), read-only only when every mount of
// it is, beside the fleet's own roots; and the named volumes, never a root.
func TestShareRecordKeepsTheHostBinds(t *testing.T) {
	aws, git := hostbind.Name("/h/.aws"), hostbind.Name("/h/.gitconfig")
	pod := &podspec.PodSpec{Mounts: []podspec.Mount{
		{Kind: podspec.MountFleet, Target: "/home/vscode/.claude", SubPath: ".claude"},
		{Kind: podspec.MountHostDir, Target: "/home/vscode/.aws", Source: "/h/.aws", SubPath: aws, Mode: 0o700},
		{Kind: podspec.MountHostDir, Target: "/opt/aws-ro", Source: "/h/.aws", SubPath: aws, ReadOnly: true},
		{Kind: podspec.MountHostFile, Target: "/home/vscode/.gitconfig", Source: "/h/.gitconfig", SubPath: git, ReadOnly: true},
		{Kind: podspec.MountSharedVolume, Target: "/cache", SubPath: podspec.SharedVolumeSubPath("cache")},
	}}
	s := &shareState{mode: topology.SingleNode, claim: "fs-alpha-share", layout: podLayout(shareLayout, 1000, 1000)}
	rec := s.record(pod)
	wantBinds := []HostBind{{Name: aws, Path: "/h/.aws"}, {Name: git, Path: "/h/.gitconfig", File: true, ReadOnly: true}}
	slices.SortFunc(wantBinds, func(a, b HostBind) int { return strings.Compare(a.Name, b.Name) })
	if !reflect.DeepEqual(rec.HostBinds, wantBinds) {
		t.Errorf("host binds %+v, want %+v", rec.HostBinds, wantBinds)
	}
	roots := map[string]podshare.SpokeRoot{}
	for _, r := range rec.Roots {
		roots[r.Name] = r
	}
	if _, ok := roots[".claude"]; !ok {
		t.Errorf("roots %+v lack the fleet's own", rec.Roots)
	}
	if r, ok := roots[aws]; !ok || len(r.Only) != 0 {
		t.Errorf("the bound folder's root: %+v", r)
	}
	if r := roots[git]; !slices.Equal(r.Only, []string{".gitconfig"}) {
		t.Errorf("the bound file's root: %+v, want only .gitconfig", r)
	}
	if _, ok := roots[hostbind.Root]; ok {
		t.Errorf("the host binds' copies were grouped into one root: %+v", rec.Roots)
	}
	if !slices.Equal(rec.Volumes, []string{strings.TrimPrefix(podspec.SharedVolumeSubPath("cache"), ".vol/")}) {
		t.Errorf("volumes %v", rec.Volumes)
	}
	if cl := rec.clone(); !reflect.DeepEqual(cl, rec) || &cl.HostBinds[0] == &rec.HostBinds[0] {
		t.Errorf("clone %+v differs or shares its slices", cl)
	}
}

// TestTeardownTakesTheLastUsersNamedVolumes: a destroy (Down) removes, before
// the instance's objects go, the fleet's named volumes only it mounted.
func TestTeardownTakesTheLastUsersNamedVolumes(t *testing.T) {
	r := newVolumeRig(t)
	r.record("api", "0000000b")
	sts := handleSTS()
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web", StatefulSet: sts, Gen: 1,
		Share: &ShareRecord{Mode: "single-node", Claim: r.claim, Volumes: []string{"0000000a", "0000000b"}}}); err != nil {
		t.Fatal(err)
	}
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, sts, 1)
	_ = newHandle(inst, r.pool).Down(inst.ContainerID)
	if !reflect.DeepEqual(r.removed, [][]string{{"0000000a"}}) {
		t.Errorf("removed %v, want the volume only the destroyed instance mounted", r.removed)
	}
}
