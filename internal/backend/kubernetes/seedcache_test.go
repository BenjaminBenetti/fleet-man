//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

const (
	cacheTestOwner   = "own123456789"
	cacheTestCluster = "minikube-abc234"
)

func isolateSeedCacheIndex(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig := SeedCacheIndexPath
	SeedCacheIndexPath = func(cluster string) string { return filepath.Join(dir, cluster, "seedcaches.json") }
	t.Cleanup(func() { SeedCacheIndexPath = orig })
}

func cacheSeedSpec() *podseed.SeedSpec {
	return &podseed.SeedSpec{
		MountDirs: []string{"/workspaces/repo", "/var/lib/docker"},
		Hostname:  "fi-x-0",
		Remap:     &podseed.Remap{User: "vscode", UID: 1000, GID: 1000},
	}
}

// The cache id is the seed's root-shaping inputs: another image, mount
// targets, remap, identity, class or disk size is another cache, while the
// workspace, the volumes' content and the hostname are not part of it.
func TestSeedCacheIDCoversTheRootsInputs(t *testing.T) {
	base := seedCacheID("d", "c", "sha256:aa", cacheSeedSpec(), false, "", "50Gi")
	same := cacheSeedSpec()
	same.MountDirs = []string{"/var/lib/docker", "/workspaces/repo"}
	same.Hostname, same.Workspace = "fi-y-0", &podseed.Workspace{}
	if got := seedCacheID("d", "c", "sha256:aa", same, false, "", "50Gi"); got != base {
		t.Fatalf("an instance's own inputs changed the id: %s vs %s", got, base)
	}
	mounts := cacheSeedSpec()
	mounts.MountDirs = append(mounts.MountDirs, "/data")
	remap := cacheSeedSpec()
	remap.Remap.UID = 1001
	origBinary := seedCacheBinary
	seedCacheBinary = func() string { return "v9.9.9" }
	otherBuild := seedCacheID("d", "c", "sha256:aa", cacheSeedSpec(), false, "", "50Gi")
	seedCacheBinary = origBinary
	for name, id := range map[string]string{
		"fleet build": otherBuild,
		"image":       seedCacheID("d", "c", "sha256:bb", cacheSeedSpec(), false, "", "50Gi"),
		"mounts":      seedCacheID("d", "c", "sha256:aa", mounts, false, "", "50Gi"),
		"remap":       seedCacheID("d", "c", "sha256:aa", remap, false, "", "50Gi"),
		"non-root":    seedCacheID("d", "c", "sha256:aa", cacheSeedSpec(), true, "", "50Gi"),
		"class":       seedCacheID("d", "c", "sha256:aa", cacheSeedSpec(), false, "csi-hostpath-sc", "50Gi"),
		"size":        seedCacheID("d", "c", "sha256:aa", cacheSeedSpec(), false, "", "20Gi"),
		"cluster":     seedCacheID("d", "other", "sha256:aa", cacheSeedSpec(), false, "", "50Gi"),
	} {
		if id == base {
			t.Errorf("another %s kept the id %s", name, id)
		}
	}
}

func cacheTestDisk() *corev1.PersistentVolumeClaim {
	class := "csi-hostpath-sc"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fi-live-a-abcd2345", Namespace: "fleet", Labels: map[string]string{names.LabelKey: "abcd2345"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			VolumeName:       "pv-1",
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}},
		},
	}
}

func cacheTestCreation(t *testing.T, automated bool, objs ...kruntime.Object) *creation {
	t.Helper()
	return &creation{
		b:        newHandle(&fleet.Instance{Name: "a", Automated: automated}, nil),
		cs:       fake.NewClientset(objs...),
		ns:       "fleet",
		owner:    cacheTestOwner,
		daemonID: "daemon",
		id:       bound{cluster: cacheTestCluster, key: "abcd2345", sts: "fi-live-a-abcd2345"},
		facts: Facts{Capabilities: &fleetgrpc.KubeCapabilities{CloneTier: "snapshot", StorageClasses: []*fleetgrpc.KubeStorageClass{
			{Name: "csi-hostpath-sc", Provisioner: "hostpath.csi.k8s.io", CsiClone: true},
			{Name: "standard", Provisioner: "k8s.io/minikube-hostpath", IsDefault: true},
		}}},
	}
}

func cachePVC(name string, ready bool, labels map[string]string) *corev1.PersistentVolumeClaim {
	if labels == nil {
		labels = names.Base(cacheTestOwner, cacheTestCluster)
		labels[names.LabelRole] = string(names.RoleSeedCache)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet", Labels: labels, UID: types.UID("uid-" + name)}}
	if ready {
		pvc.Annotations = map[string]string{names.AnnotationSeedCacheReady: "2026-10-11T00:00:00Z"}
	}
	return pvc
}

// Only an automation create, on a connection whose disks clone, uses a seed
// cache: it clones from a ready cache of its own, builds one that is not
// there (one builder per cache at a time), and otherwise seeds in full.
func TestSeedCacheFor(t *testing.T) {
	spec, image := cacheSeedSpec(), "sha256:aa"
	id := seedCacheID("daemon", cacheTestCluster, image, spec, false, "", "50Gi")
	name := names.SeedCache(id)
	t.Run("an interactive create", func(t *testing.T) {
		if p := cacheTestCreation(t, false).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("a rebuild", func(t *testing.T) {
		// Its seed resets a disk that holds the instance's workspace: a
		// ready cache is neither cloned nor built.
		c := cacheTestCreation(t, true, cachePVC(name, true, nil))
		c.gen, c.prevGen = 2, 1
		if p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
		c = cacheTestCreation(t, true)
		c.gen, c.prevGen = 2, 1
		if p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("a disk that does not clone", func(t *testing.T) {
		// A snapshot class alone (EBS) takes no PVC data source; nor does
		// the cluster's default class here, an in-tree hostpath.
		c := cacheTestCreation(t, true)
		c.facts.Capabilities.StorageClasses[0].CsiClone = false
		if p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
		disk := cacheTestDisk()
		disk.Spec.StorageClassName = nil
		if p := cacheTestCreation(t, true).seedCacheFor(context.Background(), spec, image, disk); p != nil {
			t.Fatalf("plan on the default class %+v", p)
		}
		other := "fast"
		disk.Spec.StorageClassName = &other
		if p := cacheTestCreation(t, true).seedCacheFor(context.Background(), spec, image, disk); p != nil {
			t.Fatalf("plan on a class the preflight did not find %+v", p)
		}
	})
	t.Run("a disk that clones without a snapshot class", func(t *testing.T) {
		c := cacheTestCreation(t, true)
		c.facts.Capabilities.CloneTier = "csi-clone"
		p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk())
		if p == nil || !p.build {
			t.Fatalf("plan %+v, want a build", p)
		}
		seedCaches.release(cacheTestCluster, p.id)
	})
	t.Run("clone method csi-clone", func(t *testing.T) {
		// The connection says its disks clone, on a class the preflight
		// does not know to.
		c := cacheTestCreation(t, true)
		c.rec.CloneMethod = fleet.KubeCloneCSIClone
		c.facts.Capabilities.StorageClasses = nil
		p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk())
		if p == nil || !p.build {
			t.Fatalf("plan %+v, want a build", p)
		}
		seedCaches.release(cacheTestCluster, p.id)
	})
	t.Run("clone method copy", func(t *testing.T) {
		c := cacheTestCreation(t, true)
		c.rec.CloneMethod = fleet.KubeCloneCopy
		if p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("turned off", func(t *testing.T) {
		t.Setenv(EnvSeedCache, "off")
		if p := cacheTestCreation(t, true).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("none yet: one builder", func(t *testing.T) {
		c := cacheTestCreation(t, true)
		p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk())
		if p == nil || !p.build || p.clone || p.name != name {
			t.Fatalf("plan %+v, want a build of %s", p, name)
		}
		if again := cacheTestCreation(t, true).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); again != nil {
			t.Fatalf("a second create while the first builds: %+v", again)
		}
		seedCaches.release(cacheTestCluster, p.id)
		if again := cacheTestCreation(t, true).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); again == nil || !again.build {
			t.Fatalf("after the build ended: %+v", again)
		}
		seedCaches.release(cacheTestCluster, p.id)
	})
	t.Run("ready", func(t *testing.T) {
		c := cacheTestCreation(t, true, cachePVC(name, true, nil))
		p := c.seedCacheFor(context.Background(), spec, image, cacheTestDisk())
		if p == nil || !p.clone || p.build {
			t.Fatalf("plan %+v, want a clone", p)
		}
		disk := cacheTestDisk()
		p.cloneFrom(disk)
		if ds := disk.Spec.DataSource; ds == nil || ds.Kind != "PersistentVolumeClaim" || ds.Name != name || ds.APIGroup != nil {
			t.Fatalf("data source %+v", ds)
		}
	})
	t.Run("still building", func(t *testing.T) {
		if p := cacheTestCreation(t, true, cachePVC(name, false, nil)).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("someone else's object", func(t *testing.T) {
		foreign := cachePVC(name, true, map[string]string{names.LabelManagedBy: "helm"})
		if p := cacheTestCreation(t, true, foreign).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("clones failed here lately", func(t *testing.T) {
		seedCaches.turnOff(cacheTestCluster, time.Now())
		t.Cleanup(func() { seedCaches.turnOff(cacheTestCluster, time.Time{}) })
		if p := cacheTestCreation(t, true, cachePVC(name, true, nil)).seedCacheFor(context.Background(), spec, image, cacheTestDisk()); p != nil {
			t.Fatalf("plan %+v", p)
		}
	})
}

// The cache's disk is a clone of the instance's, in its class and size, the
// connection's with the seed-cache role and no instance key.
func TestSeedCacheClaim(t *testing.T) {
	p := &seedCachePlan{id: "abc234def567", name: names.SeedCache("abc234def567"), image: "sha256:aa"}
	disk := cacheTestDisk()
	claim := seedCacheClaim(disk, p, cacheTestOwner, cacheTestCluster, "abcd2345")
	if claim.Name != p.name || claim.Namespace != "fleet" || !ownSeedCache(claim.Labels, cacheTestOwner, cacheTestCluster) {
		t.Fatalf("claim %+v", claim.ObjectMeta)
	}
	if _, ok := claim.Labels[names.LabelKey]; ok {
		t.Fatal("the cache carries an instance key: a destroy of that instance would take it")
	}
	if ds := claim.Spec.DataSource; ds == nil || ds.Name != disk.Name || ds.Kind != "PersistentVolumeClaim" {
		t.Fatalf("data source %+v", ds)
	}
	if claim.Spec.VolumeName != "" || *claim.Spec.StorageClassName != "csi-hostpath-sc" || !claim.Spec.Resources.Requests.Storage().Equal(resource.MustParse("50Gi")) {
		t.Fatalf("spec %+v", claim.Spec)
	}
	if !strings.Contains(claim.Annotations[names.AnnotationInfo], `"image":"sha256:aa"`) {
		t.Fatalf("info %q", claim.Annotations[names.AnnotationInfo])
	}
	if disk.Spec.DataSource != nil {
		t.Fatal("the instance's disk was changed")
	}
}

// A clone that binds is waited for; one whose provisioning failed, or that
// does not bind in time, is reported at once or at the deadline.
func TestAwaitClaim(t *testing.T) {
	origTimeout, origInterval := seedCacheBindTimeout, seedCacheWaitInterval
	seedCacheBindTimeout, seedCacheWaitInterval = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { seedCacheBindTimeout, seedCacheWaitInterval = origTimeout, origInterval })
	since := time.Now().Add(-time.Second)
	bound := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "fleet"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	if err := awaitClaim(context.Background(), fake.NewClientset(bound), "fleet", "d", since); err != nil {
		t.Fatalf("a bound clone: %v", err)
	}
	pending := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "fleet"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	failed := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev", Namespace: "fleet"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "d"},
		Reason:         "ProvisioningFailed", Message: "volume cloning is not supported",
		LastTimestamp: metav1.NewTime(time.Now()),
	}
	start := time.Now()
	err := awaitClaim(context.Background(), fake.NewClientset(pending, failed), "fleet", "d", since)
	if err == nil || !strings.Contains(err.Error(), "did not provision: volume cloning is not supported") || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("a failed clone: %v after %v", err, time.Since(start))
	}
	if !cloneFailure(err) {
		t.Error("a clone that did not provision is not a clone failure")
	}
	err = awaitClaim(context.Background(), fake.NewClientset(pending), "fleet", "d", since)
	if err == nil || !strings.Contains(err.Error(), "did not bind within") || !cloneFailure(err) {
		t.Fatalf("a clone that never binds: %v", err)
	}

	// A read that fails is tried again, and a wait that never read the
	// clone says so; that is no clone failure (the caches stay on).
	flaky := fake.NewClientset(bound)
	fails := 3
	flaky.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		if fails > 0 {
			fails--
			return true, nil, errors.New("etcdserver: leader changed")
		}
		return false, nil, nil
	})
	if err := awaitClaim(context.Background(), flaky, "fleet", "d", since); err != nil {
		t.Fatalf("a clone read after three failures: %v", err)
	}
	down := fake.NewClientset(bound)
	down.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})
	err = awaitClaim(context.Background(), down, "fleet", "d", since)
	if err == nil || !strings.Contains(err.Error(), "reading the clone d: etcdserver: request timed out") || cloneFailure(err) {
		t.Fatalf("a clone never read: %v (clone failure %v)", err, err != nil && cloneFailure(err))
	}
}

// A collection deletes a cache no live instance was made from for the TTL
// (the clock starting at the first look that finds none alive) and a build
// left unfinished, keeps the rest, and keeps the index in step.
func TestCollectSeedCaches(t *testing.T) {
	isolateSeedCacheIndex(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.Add(-48 * time.Hour))
	used, idle, unfinished, building := cachePVC("fi-seedcache-used", true, nil), cachePVC("fi-seedcache-idle", true, nil), cachePVC("fi-seedcache-unfinished", false, nil), cachePVC("fi-seedcache-building", false, nil)
	for _, p := range []*corev1.PersistentVolumeClaim{used, idle, unfinished} {
		p.CreationTimestamp = old
	}
	building.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	otherOwner := map[string]string{names.LabelManagedBy: names.ManagedBy, names.LabelOwner: "someoneelse1", names.LabelCluster: cacheTestCluster, names.LabelRole: string(names.RoleSeedCache)}
	foreign := cachePVC("fi-seedcache-foreign", true, otherOwner)
	foreign.CreationTimestamp = old
	cs := fake.NewClientset(used, idle, unfinished, building, foreign)
	if err := UpdateSeedCacheIndex(cacheTestCluster, func(ix *SeedCacheIndex) error {
		ix.Use(used.Name, "used", "sha256:aa", "livekey1", now.Add(-time.Hour))
		ix.Use(idle.Name, "idle", "sha256:bb", "deadkey1", now.Add(-time.Hour))
		ix.Use("fi-seedcache-gone", "gone", "sha256:cc", "livekey1", now.Add(-time.Hour))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	live := func(key string) bool { return key == "livekey1" }
	pvcs := func() []string {
		l, _ := cs.CoreV1().PersistentVolumeClaims("fleet").List(context.Background(), metav1.ListOptions{})
		var out []string
		for _, p := range l.Items {
			out = append(out, p.Name)
		}
		slices.Sort(out)
		return out
	}

	deleted, err := CollectSeedCaches(context.Background(), cs, "fleet", cacheTestOwner, cacheTestCluster, live, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{unfinished.Name}) {
		t.Fatalf("first pass deleted %v, want only the unfinished build", deleted)
	}
	ix, err := LoadSeedCacheIndex(cacheTestCluster)
	if err != nil {
		t.Fatal(err)
	}
	if e := ix.Find(idle.Name); e == nil || !e.Unreferenced.Equal(now) || len(e.Refs) != 0 {
		t.Fatalf("the idle cache's entry %+v, want its clock started", e)
	}
	if e := ix.Find(used.Name); e == nil || !e.Unreferenced.IsZero() || !slices.Equal(e.Refs, []string{"livekey1"}) {
		t.Fatalf("the used cache's entry %+v", e)
	}
	if ix.Find("fi-seedcache-gone") != nil {
		t.Fatal("an entry whose disk is gone was kept")
	}
	if e := ix.Find(building.Name); e == nil {
		t.Fatal("a cache found without an entry got none")
	}

	deleted, err = CollectSeedCaches(context.Background(), cs, "fleet", cacheTestOwner, cacheTestCluster, live, now.Add(SeedCacheTTL))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	// The build still unfinished a week on is abandoned too.
	if !slices.Equal(deleted, []string{building.Name, idle.Name}) {
		t.Fatalf("a TTL later deleted %v, want the idle cache and the abandoned build", deleted)
	}
	if got := pvcs(); !slices.Equal(got, []string{foreign.Name, used.Name}) {
		t.Fatalf("left %v", got)
	}

	// The connection goes: every cache of this daemon goes with it.
	deleted, err = DeleteSeedCaches(context.Background(), cs, "fleet", cacheTestOwner, cacheTestCluster)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{used.Name}) || !slices.Equal(pvcs(), []string{foreign.Name}) {
		t.Fatalf("removal deleted %v, left %v", deleted, pvcs())
	}
	if ix, _ = LoadSeedCacheIndex(cacheTestCluster); len(ix.Caches) != 0 {
		t.Fatalf("index after removal %+v", ix.Caches)
	}
}

// Use records a reference and restarts nothing but the clock; an index from
// a newer fleet is left alone.
func TestSeedCacheIndex(t *testing.T) {
	isolateSeedCacheIndex(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := UpdateSeedCacheIndex("c", func(ix *SeedCacheIndex) error {
		ix.Use("n", "id", "img", "k1", now)
		ix.Use("n", "id", "img", "k1", now)
		ix.Use("n", "id", "img", "k2", now.Add(time.Minute))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ix, err := LoadSeedCacheIndex("c")
	if err != nil {
		t.Fatal(err)
	}
	e := ix.Find("n")
	if e == nil || !slices.Equal(e.Refs, []string{"k1", "k2"}) || !e.Created.Equal(now) || !e.LastUsed.Equal(now.Add(time.Minute)) {
		t.Fatalf("entry %+v", e)
	}
	if err := UpdateSeedCacheIndex("c", func(ix *SeedCacheIndex) error { ix.V = SeedCacheIndexVersion + 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeedCacheIndex("c"); err != ErrSeedCacheIndexNewer {
		t.Fatalf("a newer index: %v", err)
	}
}

// A disk that does not clone from the cache (its provisioning failed) is
// replaced, with its seed Job, by an empty disk the seed fills in full; the
// connection then seeds in full for a while.
func TestSeedFromCacheFallsBackToAFullSeed(t *testing.T) {
	r := newHandleRig(t)
	origTimeout, origInterval := seedCacheBindTimeout, seedCacheWaitInterval
	seedCacheBindTimeout, seedCacheWaitInterval = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		seedCacheBindTimeout, seedCacheWaitInterval = origTimeout, origInterval
		seedCaches.turnOff(handleCluster, time.Time{})
	})
	jj, err := BeginJournalAt(filepath.Join(t.TempDir(), "job.json"), Journal{Op: OpCreate, Fleet: "alpha", Instance: "web", Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Gen: 1, Step: stepApply})
	if err != nil {
		t.Fatal(err)
	}
	c := &creation{b: newHandle(r.inst(), r.pool), p: r.pool, cs: r.cs, ns: handleNS, owner: handleOwner, jj: jj,
		id: bound{cluster: handleCluster, key: handleKey, fleet: "alpha", name: "web", sts: handleSTS()}}
	p := &seedCachePlan{id: "abc234def567", name: names.SeedCache("abc234def567"), clone: true}
	disk := cacheTestDisk()
	disk.Name, disk.Spec.VolumeName = handleSTS(), ""
	disk.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
	p.cloneFrom(disk)
	job := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: handleMeta(names.SeedJob(handleSTS(), 1))}
	o := &podspec.Objects{Disk: disk, Seed: &podspec.JobObjects{Job: job}}
	since := time.Now()
	if err := c.apply(context.Background(), KindPVC, disk.Name, true, disk); err != nil {
		t.Fatal(err)
	}
	if err := c.apply(context.Background(), KindJob, job.Name, false, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cs.CoreV1().Events(handleNS).Create(context.Background(), &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev", Namespace: handleNS},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: disk.Name},
		Reason:         "ProvisioningFailed", Message: "this driver cannot clone", LastTimestamp: metav1.NewTime(time.Now()),
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	spec := cacheSeedSpec()
	if err := c.seedFromCache(context.Background(), o, spec, p, since); err != nil {
		t.Fatalf("seedFromCache: %v", err)
	}
	if spec.Cache != "" || p.clone {
		t.Fatalf("the seed still takes the cache: spec %q plan %+v", spec.Cache, p)
	}
	got, err := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), disk.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the disk was not applied again: %v", err)
	}
	if got.Spec.DataSource != nil || got.Spec.DataSourceRef != nil {
		t.Fatalf("the replacement disk still clones: %+v", got.Spec.DataSource)
	}
	if _, err := r.cs.BatchV1().Jobs(handleNS).Get(context.Background(), job.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("the seed Job was not applied again: %v", err)
	}
	if !seedCaches.isOff(handleCluster, time.Now()) {
		t.Fatal("the connection keeps trying its clones")
	}
	var anchored bool
	for _, obj := range jj.Journal().Objects {
		if obj.Kind == KindPVC && obj.Name == disk.Name && obj.UID == string(got.UID) && obj.Anchor {
			anchored = true
		}
	}
	if !anchored {
		t.Fatalf("the journal does not hold the replacement disk: %+v", jj.Journal().Objects)
	}
}

// A clone the API would not let fleet read (a transient error, never a
// failed clone) still falls back to a full seed, but leaves the
// connection's seed caches on.
func TestSeedFromCacheKeepsCachesOnAReadError(t *testing.T) {
	r := newHandleRig(t)
	origTimeout, origInterval := seedCacheBindTimeout, seedCacheWaitInterval
	seedCacheBindTimeout, seedCacheWaitInterval = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		seedCacheBindTimeout, seedCacheWaitInterval = origTimeout, origInterval
		seedCaches.turnOff(handleCluster, time.Time{})
	})
	seedCaches.turnOff(handleCluster, time.Time{})
	jj, err := BeginJournalAt(filepath.Join(t.TempDir(), "job.json"), Journal{Op: OpCreate, Fleet: "alpha", Instance: "web", Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Gen: 1, Step: stepApply})
	if err != nil {
		t.Fatal(err)
	}
	c := &creation{b: newHandle(r.inst(), r.pool), p: r.pool, cs: r.cs, ns: handleNS, owner: handleOwner, jj: jj,
		id: bound{cluster: handleCluster, key: handleKey, fleet: "alpha", name: "web", sts: handleSTS()}}
	p := &seedCachePlan{id: "abc234def567", name: names.SeedCache("abc234def567"), clone: true}
	disk := cacheTestDisk()
	disk.Name, disk.Spec.VolumeName = handleSTS(), ""
	disk.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
	p.cloneFrom(disk)
	job := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: handleMeta(names.SeedJob(handleSTS(), 1))}
	o := &podspec.Objects{Disk: disk, Seed: &podspec.JobObjects{Job: job}}
	if err := c.apply(context.Background(), KindPVC, disk.Name, true, disk); err != nil {
		t.Fatal(err)
	}
	if err := c.apply(context.Background(), KindJob, job.Name, false, job); err != nil {
		t.Fatal(err)
	}
	// Every read of the clone fails while seedFromCache waits for it.
	start := time.Now()
	r.cs.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		if time.Since(start) < 350*time.Millisecond {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})

	spec := cacheSeedSpec()
	if err := c.seedFromCache(context.Background(), o, spec, p, start); err != nil {
		t.Fatalf("seedFromCache: %v", err)
	}
	if spec.Cache != "" || p.clone {
		t.Fatalf("the seed still takes the cache: spec %q plan %+v", spec.Cache, p)
	}
	if seedCaches.isOff(handleCluster, time.Now()) {
		t.Fatal("a read error turned the connection's seed caches off")
	}
}
