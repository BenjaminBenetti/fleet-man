//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// seedcache.go is automation's seed cache: an automation instance seeds its
// disk from a copy of its image's root, which a burst of runs of one agent
// would otherwise each make anew. Where the instance disks' CSI driver can
// clone a volume (the connection's clone tier is snapshot, and its
// clone_method is not copy), the first automation create of an image keeps
// its freshly seeded disk's root, before its pod writes anything, as the
// connection's seed cache fi-seedcache-<id12>: a clone of the disk, trimmed
// by an OpSeedCache Job to root/ alone. Later automation creates of that
// image (and seed inputs: the id covers the image, the mount targets, the
// uid remap, the identity, the class and the disk size) clone their disk
// from the cache, and their seed copies only the workspace, the volumes and
// the binary. Wherever that cannot work (no clone tier, the clone does not
// provision, or a clone the seed cannot trust) a create seeds in full, as
// every create did before; a connection whose clones fail is not tried
// again for seedCacheOffFor.
//
// The caches belong to the connection, not to an instance: they carry the
// seed-cache role and no instance key, so no destroy takes one and `fleet
// cluster gc` leaves them alone. The connection's hourly collection
// (CollectSeedCaches) deletes a cache no live instance was made from for
// SeedCacheTTL, and a build that never finished after seedCacheBuildGrace;
// removing the connection deletes them all.

// EnvSeedCache set to "off" keeps every create seeding in full.
const EnvSeedCache = "FLEET_KUBE_SEED_CACHE"

// seedCacheMount is where the trim Job mounts the cache's disk.
const seedCacheMount = "/fleet-seedcache"

var (
	// seedCacheBindTimeout bounds the wait for a clone of a disk (the cache
	// from an instance's, an instance's from the cache) to provision.
	seedCacheBindTimeout = 2 * time.Minute
	// SeedCacheTTL is how long a cache no live instance was made from stays.
	SeedCacheTTL = 7 * 24 * time.Hour
	// seedCacheBuildGrace is how long a cache's build may stay unfinished
	// before a collection takes it for abandoned.
	seedCacheBuildGrace = time.Hour
	// seedCacheOffFor is how long a connection whose clone failed seeds in
	// full.
	seedCacheOffFor = time.Hour
	// seedCacheWaitInterval paces the waits for a clone.
	seedCacheWaitInterval = DefaultWaitInterval
)

// seedCacheTracker is the process's view of the seed caches being built and
// of the connections whose clones failed.
type seedCacheTracker struct {
	mu       sync.Mutex
	building map[string]bool
	off      map[string]time.Time
}

var seedCaches = &seedCacheTracker{}

// claim makes this create the builder of cache id on cluster, unless another
// create in this daemon already is.
func (t *seedCacheTracker) claim(cluster, id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.building == nil {
		t.building = map[string]bool{}
	}
	key := cluster + "/" + id
	if t.building[key] {
		return false
	}
	t.building[key] = true
	return true
}

func (t *seedCacheTracker) release(cluster, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.building, cluster+"/"+id)
}

// turnOff stops seed caches on cluster for seedCacheOffFor.
func (t *seedCacheTracker) turnOff(cluster string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.off == nil {
		t.off = map[string]time.Time{}
	}
	t.off[cluster] = now
}

func (t *seedCacheTracker) isOff(cluster string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.off[cluster]
	return ok && now.Sub(at) < seedCacheOffFor
}

// seedCachePlan is what a create does with its image's seed cache.
type seedCachePlan struct {
	id, name, image string
	// clone: the disk is cloned from the ready cache. build: there is none
	// yet, and this create keeps its own seeded disk's root as it.
	clone, build bool
}

// seedCacheBinary names the fleet build whose pod binary seeds the root (a
// var for tests): a seed of another build may make another root, so its
// caches are its own (the old ones go with the TTL).
var seedCacheBinary = func() string { return binstore.Self().String() }

// seedCacheID is the id12 of the cache a seed of spec (with nonRoot, onto a
// disk of class and size) from image would use, by this fleet build.
func seedCacheID(daemonID, cluster, image string, spec *podseed.SeedSpec, nonRoot bool, class, size string) string {
	exclude := slices.Clone(spec.Exclude)
	slices.Sort(exclude)
	mounts := slices.Clone(spec.MountDirs)
	slices.Sort(mounts)
	remap := ""
	if r := spec.Remap; r != nil {
		remap = r.User + ":" + strconv.Itoa(r.UID) + ":" + strconv.Itoa(r.GID)
	}
	return names.SeedCacheID(daemonID, cluster, "v"+strconv.Itoa(podseed.ManifestVersion), seedCacheBinary(), image, spec.Source,
		strings.Join(exclude, "\x00"), strings.Join(mounts, "\x00"), remap, strconv.FormatBool(nonRoot), class, size)
}

// seedCacheFor is the create's seed cache plan, or nil when it seeds in full
// as any create does: only an automation create uses one, on a disk that
// CSI-clones (diskClones). A rebuild (c.prevGen set) never does: its seed
// resets a disk that already holds the instance's workspace.
func (c *creation) seedCacheFor(ctx context.Context, spec *podseed.SeedSpec, image string, disk *corev1.PersistentVolumeClaim) *seedCachePlan {
	switch {
	case !c.b.inst.Automated || c.prevGen > 0 || image == "" || os.Getenv(EnvSeedCache) == "off":
		return nil
	case c.rec.CloneMethod == fleet.KubeCloneCopy || !c.diskClones(disk):
		return nil
	case seedCaches.isOff(c.id.cluster, time.Now()):
		return nil
	}
	size := disk.Spec.Resources.Requests[corev1.ResourceStorage]
	id := seedCacheID(c.daemonID, c.id.cluster, image, spec, c.identity.NonRoot, c.diskClass(), size.String())
	plan := &seedCachePlan{id: id, name: names.SeedCache(id), image: image}
	pvc, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, plan.name, metav1.GetOptions{})
	switch {
	case gone(err):
		if !seedCaches.claim(c.id.cluster, id) {
			return nil // another create builds it; this one seeds in full
		}
		plan.build = true
	case err != nil:
		flog.Warn("kube create: reading the seed cache", "cluster", c.id.cluster, "cache", plan.name, "err", err)
		return nil
	case !ownSeedCache(pvc.Labels, c.owner, c.id.cluster):
		flog.Warn("kube create: a seed cache's name is taken by an object fleet did not make", "cluster", c.id.cluster, "cache", plan.name)
		return nil
	case pvc.DeletionTimestamp != nil || pvc.Annotations[names.AnnotationSeedCacheReady] == "":
		return nil // going, or still being built (else a collection takes it)
	default:
		plan.clone = true
	}
	return plan
}

// diskClones reports whether disk can be a CSI clone (a PVC whose dataSource
// is another PVC), which a seed cache is built and used by: its StorageClass
// clones volumes as the preflight found it (csi_clone), or the connection
// says its disks do (clone_method csi-clone). A snapshot class alone is not
// enough: a driver that only snapshots (EBS) refuses a PVC data source.
func (c *creation) diskClones(disk *corev1.PersistentVolumeClaim) bool {
	if c.rec.CloneMethod == fleet.KubeCloneCSIClone {
		return true
	}
	class := ""
	if disk.Spec.StorageClassName != nil {
		class = *disk.Spec.StorageClassName
	}
	return storageClassOf(c.facts.Capabilities, class).GetCsiClone()
}

// ownSeedCache reports whether labels are those of a seed cache this daemon
// made on connection cluster.
func ownSeedCache(labels map[string]string, owner, cluster string) bool {
	return labels[names.LabelManagedBy] == names.ManagedBy && labels[names.LabelOwner] == owner &&
		labels[names.LabelCluster] == cluster && labels[names.LabelRole] == string(names.RoleSeedCache)
}

// cloneFrom makes disk a clone of the cache's.
func (p *seedCachePlan) cloneFrom(disk *corev1.PersistentVolumeClaim) {
	disk.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: p.name}
}

// awaitClaim waits, at most seedCacheBindTimeout, for claim name, a clone,
// to bind: a ProvisioningFailed event on it (a driver that cannot clone)
// ends the wait at once. A read of the claim that fails is tried again
// until then: a wait that only ever failed to read it ends with that
// error, which is no clone failure (cloneFailure).
func awaitClaim(ctx context.Context, cs kclient.Interface, ns, name string, since time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, seedCacheBindTimeout)
	defer cancel()
	var readErr error
	read := false
	for {
		pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err != nil && ctx.Err() == nil:
			readErr = err
		case err == nil && pvc.Status.Phase == corev1.ClaimBound:
			return nil
		case err == nil:
			read = true
		}
		if msg := provisioningFailed(ctx, cs, ns, name, since); msg != "" {
			return fmt.Errorf("the clone %s did not provision: %s", name, msg)
		}
		if err := sleepCtx(ctx, seedCacheWaitInterval); err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if !read && readErr != nil {
				return fmt.Errorf("reading the clone %s: %w", name, readErr)
			}
			return fmt.Errorf("the clone %s did not bind within %s", name, seedCacheBindTimeout)
		}
	}
}

// provisioningFailed is the latest ProvisioningFailed event of claim name
// since since, or "".
func provisioningFailed(ctx context.Context, cs kclient.Interface, ns, name string, since time.Time) string {
	evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim,involvedObject.name=" + name + ",reason=ProvisioningFailed"})
	if err != nil {
		return ""
	}
	msg := ""
	for _, ev := range evs.Items {
		if !eventTime(ev).Before(since) {
			msg = ev.Message
		}
	}
	return msg
}

// uncloneDisk turns a create whose disk did not clone from the cache back
// into a full seed: its seed Job and disk are deleted and applied again,
// the disk empty.
func (c *creation) uncloneDisk(ctx context.Context, o *podspec.Objects) error {
	for _, kind := range []string{KindJob, KindPVC} {
		name := o.Seed.Job.Name
		if kind == KindPVC {
			name = o.Disk.Name
		}
		if err := deleteAndWait(ctx, c.cs, c.ns, kind, name); err != nil {
			return fmt.Errorf("replacing the disk that did not clone: %w", err)
		}
	}
	o.Disk.Spec.DataSource = nil
	if err := c.apply(ctx, KindPVC, o.Disk.Name, true, o.Disk); err != nil {
		return err
	}
	return c.apply(ctx, KindJob, o.Seed.Job.Name, false, o.Seed.Job)
}

// deleteAndWait deletes an object (foreground) and waits, at most
// seedCacheBindTimeout, for it to be gone.
func deleteAndWait(ctx context.Context, cs kclient.Interface, ns, kind, name string) error {
	k, ok := kindOf(kind)
	if !ok {
		return fmt.Errorf("no %s kind", kind)
	}
	ctx, cancel := context.WithTimeout(ctx, seedCacheBindTimeout)
	defer cancel()
	fg := metav1.DeletePropagationForeground
	if err := k.delete(ctx, cs, ns, name, metav1.DeleteOptions{PropagationPolicy: &fg}); err != nil && !gone(err) {
		return fmt.Errorf("deleting %s: %w", objectRef(kind, name), err)
	}
	for {
		if _, err := k.get(ctx, cs, ns, name); gone(err) {
			return nil
		}
		if err := sleepCtx(ctx, seedCacheWaitInterval); err != nil {
			return fmt.Errorf("%s is still there: %w", objectRef(kind, name), err)
		}
	}
}

// seedCacheClaim is the cache's disk, a clone of the instance's seeded
// disk: the instance disk's spec, the connection's labels and the
// seed-cache role, and no instance key (no destroy takes it).
func seedCacheClaim(disk *corev1.PersistentVolumeClaim, p *seedCachePlan, owner, cluster, key string) *corev1.PersistentVolumeClaim {
	labels := names.Base(owner, cluster)
	labels[names.LabelRole] = string(names.RoleSeedCache)
	info, _ := json.Marshal(struct {
		V     int    `json:"v"`
		Image string `json:"image"`
		From  string `json:"from"`
	}{1, p.image, key})
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: disk.Namespace, Labels: labels, Annotations: map[string]string{names.AnnotationInfo: string(info)}},
		Spec:       *disk.Spec.DeepCopy(),
	}
	pvc.Spec.VolumeName = ""
	pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: disk.Name}
	pvc.Spec.DataSourceRef = nil
	return pvc
}

// buildSeedCache keeps the create's freshly seeded disk's root as the cache
// p names: a clone of the disk, trimmed by an OpSeedCache Job, then marked
// ready. It runs before the instance's pod starts, so the clone holds the
// seed's root only. Best-effort: a cache that cannot be made is deleted and
// logged, and the create goes on as it would without one.
func (c *creation) buildSeedCache(ctx context.Context, adm *podspec.Admitted, r podspec.RenderInput, del *kubeimage.Delivery, p *seedCachePlan) {
	start := time.Now()
	backend.ReportStep(ctx, fleet.StepSeed, "keeping the image's root as seed cache "+p.name)
	err := c.trimIntoSeedCache(ctx, adm, r, del, p)
	if err == nil {
		c.seedCacheUse = "built " + p.name
		if err := UpdateSeedCacheIndex(c.id.cluster, func(ix *SeedCacheIndex) error {
			ix.Use(p.name, p.id, p.image, c.id.key, time.Now())
			return nil
		}); err != nil {
			flog.Warn("kube create: recording the seed cache", "cluster", c.id.cluster, "cache", p.name, "err", err)
		}
		flog.Info("kube create: seed cache kept", "cluster", c.id.cluster, "cache", p.name, "image", p.image, "ms", time.Since(start).Milliseconds())
		return
	}
	if ctx.Err() != nil {
		return // the create itself was cut; its own failure says so
	}
	flog.Warn("kube create: the seed cache could not be kept; creates of this image seed in full", "cluster", c.id.cluster, "cache", p.name, "err", err)
	if cloneFailure(err) {
		seedCaches.turnOff(c.id.cluster, time.Now())
	}
	cleanup, cancel := c.cleanupContext()
	defer cancel()
	// The trim Job first: its pod would keep the cache's disk in use.
	job := names.SeedCacheJob(c.id.sts)
	if err := deleteAndWait(cleanup, c.cs, c.ns, KindJob, job); err != nil {
		flog.Warn("kube create: deleting the seed cache's trim Job", "cluster", c.id.cluster, "job", job, "err", err)
	}
	if err := deleteAndWait(cleanup, c.cs, c.ns, KindPVC, p.name); err != nil {
		flog.Warn("kube create: deleting the unfinished seed cache", "cluster", c.id.cluster, "cache", p.name, "err", err)
	}
}

// trimIntoSeedCache is buildSeedCache's work.
func (c *creation) trimIntoSeedCache(ctx context.Context, adm *podspec.Admitted, r podspec.RenderInput, del *kubeimage.Delivery, p *seedCachePlan) error {
	claim := seedCacheClaim(adm.Objects.Disk, p, c.owner, c.id.cluster, c.id.key)
	body, err := podspec.ApplyBody(claim)
	if err != nil {
		return err
	}
	since := time.Now()
	if _, err := applyObject(ctx, c.cs, c.ns, KindPVC, p.name, body); err != nil {
		return fmt.Errorf("applying the seed cache's disk: %w", err)
	}
	job, err := podspec.RenderJob(adm.Result, r, podspec.JobInput{
		Name:           names.SeedCacheJob(c.id.sts),
		Claims:         []podspec.JobClaim{{ClaimName: p.name, Path: seedCacheMount}},
		ActiveDeadline: DefaultDeadlines().SeedDeadline(0),
	}, names.RoleSeed)
	if err != nil {
		return err
	}
	// The Job is the instance's (its key, its journal): a destroy or the
	// reconciler takes it with the instance's other objects.
	if err := c.apply(ctx, KindJob, job.Job.Name, false, job.Job); err != nil {
		return err
	}
	if err := awaitClaim(ctx, c.cs, c.ns, p.name, since); err != nil {
		return err
	}
	want := job.Job.Spec.Template.Spec
	deadlines := DefaultDeadlines()
	deadlines.PVCBind = seedCacheBindTimeout
	res, err := c.p.RunJob(ctx, JobRun{
		Cluster:   c.id.cluster,
		Job:       job.Job.Name,
		Spec:      podseed.JobSpec{Op: podseed.OpSeedCache, Gen: c.gen, PVC: seedCacheMount, Cache: p.id},
		PVC:       p.name,
		Deadlines: &deadlines,
		Check: func(pod *corev1.Pod) error {
			_, err := checkJobPod(pod, &want, del, []string{del.Digest, del.ImageID, c.nodeImage})
			return err
		},
	})
	switch {
	case err != nil:
		return classify(err)
	case !res.OK:
		return fmt.Errorf("the seed cache's trim failed: %s", res.Error)
	}
	ready := time.Now().UTC().Format(time.RFC3339)
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Patch(ctx, p.name, types.MergePatchType, annotationPatch(names.AnnotationSeedCacheReady, &ready), metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		return fmt.Errorf("marking the seed cache ready: %w", err)
	}
	return nil
}

// cloneFailure reports whether err says a clone did not provision or came
// out empty: the connection's driver does not clone after all.
func cloneFailure(err error) bool {
	msg := err.Error()
	var de *DeadlineError
	return errors.As(err, &de) || strings.Contains(msg, "did not provision") || strings.Contains(msg, "did not bind") || strings.Contains(msg, "no seeded root")
}

// seedFromCache sets a create whose disk cloned from the cache up to seed
// from it: the clone must provision first, and a disk that does not is
// replaced by an empty one, which is seeded in full.
func (c *creation) seedFromCache(ctx context.Context, o *podspec.Objects, spec *podseed.SeedSpec, p *seedCachePlan, since time.Time) error {
	err := awaitClaim(ctx, c.cs, c.ns, o.Disk.Name, since)
	if err == nil {
		spec.Cache = p.id
		c.seedCacheUse = "cloned from " + p.name
		if err := UpdateSeedCacheIndex(c.id.cluster, func(ix *SeedCacheIndex) error {
			ix.Use(p.name, p.id, p.image, c.id.key, time.Now())
			return nil
		}); err != nil {
			flog.Warn("kube create: recording the seed cache's use", "cluster", c.id.cluster, "cache", p.name, "err", err)
		}
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
	flog.Warn("kube create: the disk did not clone from the seed cache; seeding in full", "cluster", c.id.cluster, "cache", p.name, "err", err)
	// Only a clone that failed says the driver does not clone; one the API
	// would not let fleet look at (a transient error) says nothing of it.
	if cloneFailure(err) {
		seedCaches.turnOff(c.id.cluster, time.Now())
	}
	p.clone = false
	return c.uncloneDisk(ctx, o)
}

// CollectSeedCaches deletes connection cluster's seed caches (in namespace
// ns, owner's) that no live instance (live reports an instance key's) was
// made from for SeedCacheTTL, and the builds left unfinished for
// seedCacheBuildGrace; it keeps the index in step and returns the names
// deleted. The caller holds the connection's owner Lease.
func CollectSeedCaches(ctx context.Context, cs kclient.Interface, ns, owner, cluster string, live func(key string) bool, now time.Time) ([]string, error) {
	return collectSeedCaches(ctx, cs, ns, owner, cluster, func(e *SeedCacheEntry) bool { return e.expired(live, now, SeedCacheTTL) }, now)
}

// DeleteSeedCaches deletes every seed cache of connection cluster, which is
// being removed, and forgets them.
func DeleteSeedCaches(ctx context.Context, cs kclient.Interface, ns, owner, cluster string) ([]string, error) {
	return collectSeedCaches(ctx, cs, ns, owner, cluster, func(*SeedCacheEntry) bool { return true }, time.Now())
}

func collectSeedCaches(ctx context.Context, cs kclient.Interface, ns, owner, cluster string, drop func(*SeedCacheEntry) bool, now time.Time) ([]string, error) {
	list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
		LabelSelector: names.Selector(owner, cluster) + "," + names.LabelRole + "=" + string(names.RoleSeedCache),
	})
	if err != nil {
		return nil, err
	}
	var deleted []string
	var errs []error
	err = UpdateSeedCacheIndex(cluster, func(ix *SeedCacheIndex) error {
		present := map[string]bool{}
		for _, pvc := range list.Items {
			if pvc.DeletionTimestamp != nil {
				continue
			}
			present[pvc.Name] = true
			e := ix.Find(pvc.Name)
			if e == nil {
				// Not recorded (an index lost with a restore, or a build
				// cut before it recorded): unreferenced from now on.
				ix.Caches = append(ix.Caches, SeedCacheEntry{Name: pvc.Name, Created: pvc.CreationTimestamp.UTC()})
				e = &ix.Caches[len(ix.Caches)-1]
			}
			unfinished := pvc.Annotations[names.AnnotationSeedCacheReady] == "" && now.Sub(pvc.CreationTimestamp.Time) >= seedCacheBuildGrace
			if !unfinished && !drop(e) {
				continue
			}
			fg := metav1.DeletePropagationBackground
			if err := cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvc.Name, metav1.DeleteOptions{PropagationPolicy: &fg, Preconditions: &metav1.Preconditions{UID: &pvc.UID}}); err != nil && !gone(err) {
				errs = append(errs, fmt.Errorf("deleting seed cache %s: %w", pvc.Name, err))
				continue
			}
			deleted = append(deleted, pvc.Name)
			ix.Remove(pvc.Name)
			delete(present, pvc.Name)
		}
		ix.Caches = slices.DeleteFunc(ix.Caches, func(e SeedCacheEntry) bool { return !present[e.Name] })
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return deleted, joinErrors(errs)
}
