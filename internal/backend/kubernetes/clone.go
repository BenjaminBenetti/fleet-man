//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// clone.go is an instance's clone on its cluster (CloneContext), docker
// commit and run on a cluster: a new instance with the source's root
// generation (its packages, its $HOME), its workspace (uncommitted work
// included) and its pod, under the clone's own name.
//
//  1. The source is read and verified against fleetd's record of it: the
//     StatefulSet fleet created, the pod template fleet rendered, no job of
//     its own in progress. Its disk's storage and the connection's clone
//     settings choose the tier (selectCloneTier).
//  2. The clone's journal is begun, then its disk is made by the tier:
//     restored from a VolumeSnapshot of the source's disk (T1), a CSI clone
//     of a stopped source's disk (T1b), or copied by a copier Job beside the
//     source (T2). The point the copy is taken at is held still: a snapshot's
//     cut and a copier's final delta freeze the source's user processes
//     where the pod can be frozen (privileged pods: their fleet-user cgroup;
//     the agent and the thaw watchdog stay live), or stop the source with
//     clone_consistency stop-source. A source that can be neither is copied
//     live, and the clone says so in its banner (best-effort). A restored
//     disk's fix-ups (the clone's hostname, its volumes reset to the image's
//     copy-up, none of the source's logs) run in a Job too.
//  3. The clone's Secrets and StatefulSet are the source's, renamed
//     (podspec.CloneObjects): the same image, security context and
//     container-created time, so its pod is the source's pod and its
//     lifecycle hooks' markers, which the disk carried over, hold. Its pod
//     is waited for, verified and given its agent; the hooks do not run
//     again (as docker commit and run does not run them).
//
// The share PVC is never snapshotted: shared state stays the fleet's. A clone
// that fails leaves its disk and Secrets labelled by its key for a destroy,
// and deletes its copier Jobs and snapshots; one the daemon dies in is
// failed at the next start, whose reconciler deletes those and starts a
// source it had stopped (UndoClone).

// The steps a clone journals, beside the create's.
const (
	stepCloneSnapshot Step = "snapshot"
	stepCloneCopy     Step = "copy"
)

// Clone bounds; variables so tests can shorten them.
var (
	// cloneFreeze is how long a freeze of the source may last before the
	// pod's watchdog thaws it.
	cloneFreeze = podwire.DefaultFreezeWatchdog
	// cloneCutWait bounds a frozen source's wait for its snapshot's cut:
	// what is left of the freeze's watchdog, less a margin.
	cloneCutWait = cloneFreeze - 5*time.Second
	// copierResources are a copier's requests: it runs beside the running
	// source, on its node.
	copierResources = corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}}
)

// The source disk's mount path in a copier.
const cloneSourcePath = "/fleet-src"

// The interfaces the handle provides for a clone.
var (
	_ backend.CloneContexter = (*Backend)(nil)
	_ backend.WorkspaceOwner = (*Backend)(nil)
)

// OwnsWorkspace is true: a cluster instance's workspace is on its disk, and
// a clone copies it with the rest of the disk.
func (b *Backend) OwnsWorkspace() bool { return true }

// Clone is CloneContext without a job's context.
func (b *Backend) Clone(sourceContainerID, destWorkspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return b.CloneContext(context.Background(), sourceContainerID, backend.UpRequest{WorkspaceDir: destWorkspaceDir, Mounts: mounts})
}

// CloneContext clones the instance sourceContainerID names into the bound
// (CLONING) instance: see clone.go. ctx is the clone job's: cancelling it (a
// destroy of the clone, the daemon shutting down) stops it at its next step.
// The fleet's mounts (req.Mounts) are the source's already: the clone's pod
// is the source's.
func (b *Backend) CloneContext(ctx context.Context, sourceContainerID string, req backend.UpRequest) (*backend.UpResult, error) {
	id, err := b.identity()
	if err != nil {
		return nil, err
	}
	c := &cloning{creation: &creation{b: b, p: b.p(), id: id, env: currentEnvironment(), req: req, start: time.Now(), verb: "clone"}, srcID: sourceContainerID}
	res, err := c.run(ctx)
	if err != nil {
		flog.Error("kube clone failed", "fleet", id.fleet, "instance", id.name, "src", sourceContainerID, "cluster", id.cluster, "key", id.key, "ms", time.Since(c.start).Milliseconds(), "err", err)
		return nil, err
	}
	flog.Info("kube clone: instance running", "fleet", id.fleet, "instance", id.name, "src", sourceContainerID, "tier", c.plan.Tier, "hold", c.plan.Hold,
		"best_effort", c.bestEffort != "", "container", res.ContainerID, "ms", time.Since(c.start).Milliseconds())
	return res, nil
}

// cloning is one clone in progress: the clone's job (creation: its identity,
// connection, journal and objects) and its source.
type cloning struct {
	*creation
	srcID string

	src     where
	srcRec  InstanceRecord
	srcSet  *appsv1.StatefulSet
	srcDisk *corev1.PersistentVolumeClaim
	srcSpec *corev1.Secret
	srcEnv  *corev1.Secret
	running bool
	srcNode string
	plan    ClonePlan

	// held is the hold on the source in force; bestEffort, when set, says
	// why the clone is not of one point in time.
	mu         sync.Mutex
	frozen     bool
	stopped    bool
	restarting chan error
	bestEffort string
	// transient are the clone's own objects that go once it is done (its
	// snapshot, its copier Job), by kind.
	transient []JournalObject
}

func (c *cloning) run(ctx context.Context) (*backend.UpResult, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	if err := c.loadSource(ctx); err != nil {
		return nil, err
	}
	facts, err := c.cloneFacts(ctx)
	if err != nil {
		return nil, err
	}
	if c.plan, err = selectCloneTier(facts); err != nil {
		return nil, err
	}
	c.gen = c.srcRec.Gen
	backend.ReportStep(ctx, fleet.StepBackendClone, fmt.Sprintf("%s (%s)", c.plan.Tier, c.plan.Why))

	jj, err := BeginJournal(Journal{Op: OpClone, Fleet: c.id.fleet, Instance: c.id.name, Cluster: c.id.cluster, Namespace: c.ns, Key: c.id.key, Gen: c.gen, Step: stepPreflight,
		Source: &JournalSource{StatefulSet: c.src.sts, Key: c.srcRec.Key, UID: c.srcRec.StatefulSetUID}})
	if errors.Is(err, ErrJournalExists) {
		return nil, kubeerr.New(kubeerr.Interrupted, "an earlier job of this instance left its journal behind; destroy the instance and clone again")
	}
	if err != nil {
		return nil, err
	}
	c.jj = jj
	defer c.finish()

	res, err := c.provision(ctx)
	if err != nil {
		return nil, c.failedClone(ctx, err)
	}
	if err := c.waitRestart(); err != nil {
		c.warn(ctx, fmt.Sprintf("the clone's source did not start again after the clone: %v", err))
	}
	c.deleteTransient()
	res.Warnings = c.warningLines()
	return res, nil
}

// failedClone ends a clone that failed with err: the source is let go (thawed,
// or started again), the clone's pod scaled down and its copiers and
// snapshots deleted. One the daemon's shutdown cut is left at a checkpoint
// (its journal names what to undo at the next start), the source let go all
// the same while the pool lasts.
func (c *cloning) failedClone(ctx context.Context, err error) error {
	c.letGo()
	if backend.ShutdownCut(ctx) {
		return c.checkpoint()
	}
	if werr := c.waitRestart(); werr != nil {
		flog.Warn("kube clone: the source did not start again", "fleet", c.id.fleet, "src", c.src.sts, "err", werr)
	}
	c.stopFailed()
	c.deleteTransient()
	return err
}

// loadSource reads the source instance and checks it is the one fleetd made
// and recorded, at rest from any job of its own.
func (c *cloning) loadSource(ctx context.Context) error {
	id, ok := fleet.ParseKubeContainerID(c.srcID)
	if !ok {
		return kubeerr.New(kubeerr.Unsupported, "the source %q is not an instance on a cluster: a cluster instance clones only another on its cluster", c.srcID)
	}
	if id.Cluster != c.id.cluster {
		return kubeerr.New(kubeerr.Unsupported, "the source runs on another connection (%s) than its clone (%s)", id.Cluster, c.id.cluster)
	}
	c.src = where{cluster: id.Cluster, namespace: id.Namespace, sts: id.StatefulSet, gen: id.Generation}
	if c.src.namespace != "" && c.src.namespace != c.ns {
		return kubeerr.New(kubeerr.Unsupported, "the source is in namespace %s, its connection's is %s now", c.src.namespace, c.ns)
	}
	set, err := c.cs.AppsV1().StatefulSets(c.ns).Get(ctx, c.src.sts, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the source's StatefulSet %s: %w", c.src.sts, err))
	}
	key := set.Labels[names.LabelKey]
	if set.Labels[names.LabelOwner] != c.owner || set.Labels[names.LabelCluster] != c.id.cluster || !names.ValidHash(key) {
		return &TamperedError{Namespace: c.ns, Pod: names.Pod(c.src.sts), Reasons: []string{"its StatefulSet is not one this daemon made on this connection"}}
	}
	rec, err := LoadInstanceRecord(c.id.cluster, key)
	if err != nil {
		return fmt.Errorf("fleet's record of the source: %w", err)
	}
	var reasons []string
	if rec.StatefulSet != set.Name || string(set.UID) != rec.StatefulSetUID {
		reasons = append(reasons, fmt.Sprintf("StatefulSet %s has uid %s, fleet created %s", set.Name, set.UID, rec.StatefulSetUID))
	}
	if !managedBy(set.ManagedFields, DefaultFieldManager) {
		reasons = append(reasons, "its StatefulSet was not applied by fleet")
	}
	if podspec.SpecHash(&set.Spec.Template.Spec) != rec.SpecHash {
		reasons = append(reasons, "its pod template differs from fleet's render")
	}
	if len(reasons) > 0 {
		return &TamperedError{Namespace: c.ns, Pod: names.Pod(c.src.sts), Reasons: reasons}
	}
	if mark := set.Annotations[names.AnnotationOp]; mark != "" {
		return kubeerr.New(kubeerr.Unavailable, "the source is in the middle of a job of its own (%s): clone it once that is done", mark)
	}
	if podspec.GenOf(set) != rec.Gen {
		return kubeerr.New(kubeerr.Unavailable, "the source's StatefulSet is at root generation %d, fleet's record at %d", podspec.GenOf(set), rec.Gen)
	}
	disk, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, c.src.sts, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the source's disk %s: %w", c.src.sts, err))
	}
	spec, err := c.cs.CoreV1().Secrets(c.ns).Get(ctx, names.SpecSecret(c.src.sts, rec.Gen), metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the source's spec Secret: %w", err))
	}
	env, err := c.cs.CoreV1().Secrets(c.ns).Get(ctx, names.EnvSecret(c.src.sts, rec.Gen), metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the source's env Secret: %w", err))
	}
	c.srcRec, c.srcSet, c.srcDisk, c.srcSpec, c.srcEnv = rec, set, disk, spec, env
	c.running = set.Spec.Replicas != nil && *set.Spec.Replicas > 0
	if c.running {
		pod, err := c.p.Verify(ctx, rec.target())
		if err != nil {
			return classify(fmt.Errorf("the running source's pod: %w", err))
		}
		c.srcNode = pod.Spec.NodeName
	}
	return nil
}

// cloneFacts is what the tier is chosen from: the connection's clone
// settings and the source disk's storage.
func (c *cloning) cloneFacts(ctx context.Context) (CloneFacts, error) {
	f := CloneFacts{Method: c.rec.CloneMethod, Consistency: c.rec.CloneConsistency, SourceRunning: c.running, SnapshotClassSet: c.rec.SnapshotClass}
	class := ""
	if c.srcDisk.Spec.StorageClassName != nil {
		class = *c.srcDisk.Spec.StorageClassName
	}
	var known *fleetgrpc.KubeStorageClass
	for _, sc := range c.facts.Capabilities.GetStorageClasses() {
		if sc.GetName() == class || (class == "" && sc.GetIsDefault()) {
			known = sc
		}
	}
	switch {
	case known != nil:
		f.Driver, f.CSIClone = known.GetProvisioner(), known.GetCsiClone()
	case class != "":
		if sc, err := c.cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{}); err == nil {
			f.Driver = sc.Provisioner
		}
	}
	if f.Driver != "" {
		if _, err := c.cs.StorageV1().CSIDrivers().Get(ctx, f.Driver, metav1.GetOptions{}); gone(err) {
			f.Driver = "" // not a CSI volume: no snapshot or clone of it is fleet's to take
		}
	}
	f.CSIClone = f.CSIClone || preflight.ClonesVolumes(f.Driver)
	f.SnapshotClass = snapshotClassOf(ctx, c.cs, f.Driver, c.rec.SnapshotClass)
	return f, nil
}

// provision makes the clone after its journal began.
func (c *cloning) provision(ctx context.Context) (*backend.UpResult, error) {
	var source *corev1.TypedLocalObjectReference
	size := ""
	switch c.plan.Tier {
	case TierSnapshot:
		snap, restore, err := c.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		source, size = snapshotSource(snap), restore
	case TierCSIClone:
		if err := c.step(ctx, stepCloneCopy, fleet.StepBackendClone, "holding the source still"); err != nil {
			return nil, err
		}
		if err := c.hold(ctx); err != nil {
			return nil, err
		}
		source = pvcSource(c.srcDisk.Name)
	}

	if err := c.step(ctx, stepApply, fleet.StepApplyManifests, ""); err != nil {
		return nil, err
	}
	o, err := podspec.CloneObjects(podspec.CloneSource{StatefulSet: c.srcSet, SpecSecret: c.srcSpec, EnvSecret: c.srcEnv, Disk: c.srcDisk, Gen: c.gen}, c.cloneInput(source, size))
	if err != nil {
		return nil, err
	}
	if err := c.apply(ctx, KindPVC, o.Disk.Name, true, o.Disk); err != nil {
		return nil, err
	}
	for _, s := range []*corev1.Secret{o.SpecSecret, o.EnvSecret} {
		if err := c.apply(ctx, KindSecret, s.Name, false, s); err != nil {
			return nil, err
		}
	}

	if err := c.step(ctx, stepCloneCopy, fleet.StepSeed, string(c.plan.Tier)); err != nil {
		return nil, err
	}
	if err := c.copyDisk(ctx, o); err != nil {
		return nil, err
	}
	c.letGo()

	if err := c.step(ctx, stepStart, fleet.StepWaitPod, ""); err != nil {
		return nil, err
	}
	return c.startClone(ctx, o)
}

// cloneInput is who the clone is, for its objects.
func (c *cloning) cloneInput(source *corev1.TypedLocalObjectReference, size string) podspec.CloneInput {
	rec := c.rec
	return podspec.CloneInput{
		Cluster: &rec, Namespace: c.ns, Name: c.id.sts, Key: c.id.key, Owner: c.owner, ClusterID: c.id.cluster,
		FleetName: c.id.fleet, FleetHash: names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet), InstanceName: c.id.name,
		WorkspaceDir: c.req.WorkspaceDir, Replicas: 1, DiskSource: source, DiskSize: size,
	}
}

// snapshot takes the source disk's snapshot at a point the source is held
// still for, and waits for it to be ready to restore from. It returns the
// snapshot and its restore size.
func (c *cloning) snapshot(ctx context.Context) (string, string, error) {
	if err := c.step(ctx, stepCloneSnapshot, fleet.StepBackendClone, "snapshot of the source's disk"); err != nil {
		return "", "", err
	}
	name := names.CloneSnapshot(c.id.sts, c.gen)
	labels := c.cloneInput(nil, "").Labels(names.RoleCopier)
	body, err := snapshotBody(name, c.ns, c.plan.SnapshotClass, c.srcDisk.Name, labels)
	if err != nil {
		return "", "", err
	}
	if err := c.hold(ctx); err != nil {
		return "", "", err
	}
	if !c.isHeld() && c.running {
		// Flushed at least: a snapshot of a live block disk is what a crash
		// leaves, which its journal recovers.
		c.syncSource(ctx)
	}
	if err := c.applyTransient(ctx, KindVolumeSnapshot, name, body); err != nil {
		return "", "", err
	}
	d := DefaultDeadlines()
	deadline := time.Now().Add(d.Snapshot)
	cutBy := time.Now().Add(cloneCutWait)
	for {
		v, err := readSnapshot(ctx, c.cs, c.ns, name)
		if err != nil && !gone(err) {
			return "", "", classify(fmt.Errorf("reading the source's snapshot %s: %w", name, err))
		}
		var st *snapshotStatus
		if v != nil {
			st = v.Status
		}
		if msg := st.failure(); msg != "" && !st.ready() {
			return "", "", fmt.Errorf("the snapshot of the source's disk failed: %s", msg)
		}
		if st.cut() || st.ready() {
			c.letGo()
		} else if c.isFrozen() && time.Now().After(cutBy) {
			c.letGo()
			c.markBestEffort(ctx, "the snapshot was not cut within the source's freeze; it is of the running disk")
		}
		if st.ready() {
			return name, st.RestoreSize, nil
		}
		if time.Now().After(deadline) {
			c.letGo()
			return "", "", &DeadlineError{Kind: CodeTimeout, Wait: "the snapshot " + name + " of the source's disk to be ready", After: d.Snapshot, Env: EnvSnapshotTimeout}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			c.letGo()
			return "", "", fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
	}
}

// copyDisk fills the clone's disk: a copier Job copies the source's (T2), or
// a Job fixes up the disk a snapshot or CSI clone restored.
func (c *cloning) copyDisk(ctx context.Context, o *podspec.Objects) error {
	spec := podspec.CloneSpecOf(o)
	job := podspec.JobInput{Name: names.CopierJob(c.id.sts, c.gen), ActiveDeadline: DefaultDeadlines().Copy}
	live := false
	if c.plan.Tier == TierCopy {
		spec.Source = cloneSourcePath
		live = c.running
		spec.Live = live
		job.Claims = []podspec.JobClaim{{ClaimName: c.srcDisk.Name, Path: cloneSourcePath, ReadOnly: true}}
		if c.running {
			// Beside the running source: its disk is attached to its node.
			job.Node, job.Resources = c.srcNode, copierResources.DeepCopy()
		}
	}
	if job.Resources == nil {
		// The Job binds the clone's disk where it lands: where the clone must
		// fit.
		dev := c.srcSet.Spec.Template.Spec.Containers[0]
		job.Resources = dev.Resources.DeepCopy()
	}
	jo, err := podspec.CloneJob(c.srcSet, c.cloneInput(nil, ""), c.gen, job, names.RoleCopier)
	if err != nil {
		return err
	}
	if err := c.applyTransientObject(ctx, KindJob, jo.Job.Name, jo.Job); err != nil {
		return err
	}
	want := jo.Job.Spec.Template.Spec
	digests := slices.Clone(c.srcRec.ImageDigests)
	run := JobRun{
		Cluster: c.id.cluster, Job: jo.Job.Name, PVC: o.Disk.Name,
		Spec: podseed.JobSpec{Op: podseed.OpClone, Gen: c.gen, PVC: podspec.JobDiskPath, Clone: &spec},
		Progress: func(pr jobboot.Progress) {
			backend.ReportStep(ctx, fleet.StepSeed, seedProgress(pr))
		},
		Check: func(pod *corev1.Pod) error {
			if podspec.SpecHash(&pod.Spec) != podspec.SpecHash(&want) {
				return &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: []string{"its spec differs from fleet's render"}}
			}
			if st := containerStatus(pod, podspec.JobContainer); st != nil && st.ImageID != "" && !digestIn(st.ImageID, digests) {
				return &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: []string{fmt.Sprintf("its %s container runs %s, not the source's image", podspec.JobContainer, st.ImageID)}}
			}
			return nil
		},
		Pending: c.diskRefused(o.Disk.Name),
	}
	if live {
		run.Barrier = c.barrier
	}
	res, err := c.p.RunJob(ctx, run)
	if err != nil {
		return classify(err)
	}
	if !res.OK {
		if strings.Contains(res.Error, podseed.ErrNoInstance.Error()) && c.plan.Tier != TierCopy {
			return kubeerr.New(kubeerr.Misconfigured, "the clone's disk, restored by %s from the source's, came up empty: its driver did not restore it; set clone_method to copy", c.plan.Tier)
		}
		return fmt.Errorf("the clone's disk: %s", res.Error)
	}
	m, err := podseed.ParseManifest(res.Output)
	if err != nil {
		return fmt.Errorf("the clone's manifest: %w", err)
	}
	for _, w := range m.Warnings {
		flog.Info("kube clone: copier note", "fleet", c.id.fleet, "instance", c.id.name, "note", w)
	}
	return nil
}

// diskRefused is a copier Job's Pending check: a disk restored from a
// snapshot or cloned that the provisioner refused (a driver that cannot
// restore or clone it) fails at once, naming why.
func (c *cloning) diskRefused(pvc string) func(context.Context) error {
	if c.plan.Tier == TierCopy {
		return nil
	}
	return func(ctx context.Context) error {
		evs, err := c.cs.CoreV1().Events(c.ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim,involvedObject.name=" + pvc + ",reason=ProvisioningFailed"})
		if err != nil {
			return nil
		}
		for _, ev := range evs.Items {
			if eventTime(ev).Before(c.start) {
				continue
			}
			msg := strings.ToLower(ev.Message)
			if strings.Contains(msg, "data source") || strings.Contains(msg, "datasource") || strings.Contains(msg, "clon") || strings.Contains(msg, "snapshot") {
				return kubeerr.New(kubeerr.Misconfigured, "the provisioner did not make the clone's disk from the source's (%s): %s; set clone_method to copy", c.plan.Tier, ev.Message)
			}
		}
		return nil
	}
}

// startClone starts the clone's pod and waits for it, verified, with its
// agent.
func (c *cloning) startClone(ctx context.Context, o *podspec.Objects) (*backend.UpResult, error) {
	src := c.srcRec
	rec := InstanceRecord{
		Cluster: c.id.cluster, Namespace: c.ns, Key: c.id.key, Fleet: c.id.fleet, Instance: c.id.name,
		StatefulSet: c.id.sts, Gen: c.gen, SpecHash: podspec.SpecHash(&o.StatefulSet.Spec.Template.Spec),
		Image: src.Image, ImageDigests: slices.Clone(src.ImageDigests), Strategy: src.Strategy, RemoteUser: src.RemoteUser,
		WorkspaceFolder: src.WorkspaceFolder, NonRoot: src.NonRoot, Created: src.Created,
	}
	if images := c.env.Images; images != nil {
		if _, err := images.Share(c.id.cluster, src.Key, c.id.key); err != nil {
			c.warn(ctx, fmt.Sprintf("recording the clone's use of the source's image: %v", err))
		}
	}
	c.started = true
	uid, err := c.applyUID(ctx, KindStatefulSet, o.StatefulSet.Name, false, o.StatefulSet)
	if err != nil {
		return nil, err
	}
	rec.StatefulSetUID = uid
	if pdb := o.DisruptionBudget; pdb != nil {
		if err := c.apply(ctx, KindPDB, pdb.Name, false, pdb); err != nil {
			return nil, err
		}
	}
	if err := SaveInstanceRecord(rec); err != nil {
		return nil, err
	}
	failedCreate := c.failedCreate(o.StatefulSet.Name)
	pod, err := c.p.WaitReady(ctx, c.id.cluster, c.id.sts, WaitOptions{PVC: o.Disk.Name, Check: failedCreate})
	if err != nil {
		return nil, classify(err)
	}
	if st := containerStatus(pod, DevContainer); st != nil && st.ImageID != "" && !digestIn(st.ImageID, rec.ImageDigests) {
		return nil, &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: []string{fmt.Sprintf("its %s container runs %s, not the source's image", DevContainer, st.ImageID)}}
	}
	if err := c.step(ctx, stepAgent, fleet.StepWaitPod, "the pod agent"); err != nil {
		return nil, err
	}
	target := rec.target()
	target.Priority = PriorityInteractive
	sess, err := c.p.EnsureAgent(ctx, target)
	if err != nil {
		return nil, classify(err)
	}
	if ready := backend.HooksFrom(ctx).SharedStateReady; ready != nil {
		if err := ready(ctx); err != nil {
			c.warn(ctx, fmt.Sprintf("shared state is not in sync yet: %v", err))
		}
	}
	if err := ensureTmuxOn(ctx, sess); err != nil {
		c.warn(ctx, fmt.Sprintf("tmux for the instance's sessions: %v", err))
	}
	return &backend.UpResult{Outcome: "success", ContainerID: ContainerID(c.id.cluster, c.ns, c.id.sts, c.gen), RemoteUser: src.RemoteUser, RemoteWorkspaceFolder: src.WorkspaceFolder}, nil
}

// --- holding the source still ---

// barrier is a live copier's barrier: before its final delta the source is
// held still, after it let go.
func (c *cloning) barrier(ctx context.Context, b jobboot.Barrier) error {
	switch b.Name {
	case podseed.BarrierFinal:
		backend.ReportStep(ctx, fleet.StepSeed, "final delta: holding the source still")
		return c.hold(ctx)
	case podseed.BarrierCopied:
		flog.Info("kube clone: copied", "fleet", c.id.fleet, "instance", c.id.name, "detail", b.Detail)
		c.letGo()
		return nil
	}
	return fmt.Errorf("a clone's copier has no barrier %q", b.Name)
}

// hold holds the running source still as the plan says: its user processes
// frozen (a pod that cannot be is copied live, and the clone is
// best-effort), or the source stopped. A stopped source needs nothing.
func (c *cloning) hold(ctx context.Context) error {
	if !c.running {
		return nil
	}
	switch c.plan.Hold {
	case HoldFreeze:
		c.freeze(ctx)
		return nil
	case HoldStopSource:
		return c.stopSource(ctx)
	}
	return nil
}

func (c *cloning) isHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frozen || c.stopped
}

func (c *cloning) isFrozen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frozen
}

// sourceSession is the running source's agent session.
func (c *cloning) sourceSession(ctx context.Context) (*AgentSession, error) {
	t := c.srcRec.target()
	t.Priority = PriorityInteractive
	s, err := c.p.EnsureAgent(ctx, t)
	return s, classify(err)
}

// freeze freezes the source's user processes; where it cannot, the clone
// goes on live and is best-effort.
func (c *cloning) freeze(ctx context.Context) {
	sess, err := c.sourceSession(ctx)
	if err == nil {
		var info podwire.FreezeInfo
		err = sess.Client().Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: cloneFreeze.Milliseconds()}, &info)
		if err == nil {
			c.mu.Lock()
			c.frozen = true
			c.mu.Unlock()
			flog.Info("kube clone: froze the source", "fleet", c.id.fleet, "src", c.src.sts, "settle_ms", info.SettleMS, "sync_ms", info.SyncMS)
			return
		}
	}
	why := "its pod cannot be frozen"
	switch {
	case podwire.IsCode(err, podwire.CodeUnsupported):
		why = "its pod is not privileged, so it cannot be frozen"
	case err != nil:
		why = "freezing it failed: " + err.Error()
	}
	c.markBestEffort(ctx, "the source ran on while it was copied ("+why+"); files written meanwhile may differ (set clone_consistency to stop-source to stop it instead)")
}

// syncSource flushes the running source's disk.
func (c *cloning) syncSource(ctx context.Context) {
	sess, err := c.sourceSession(ctx)
	if err == nil {
		err = sess.Client().Call(ctx, podwire.MethodSync, nil, nil)
	}
	if err != nil {
		flog.Warn("kube clone: flushing the source's disk", "fleet", c.id.fleet, "src", c.src.sts, "err", err)
	}
}

// stopSource scales the source to 0 for the final copy
// (clone_consistency stop-source), journaled first so an interrupted clone
// starts it again.
func (c *cloning) stopSource(ctx context.Context) error {
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return nil
	}
	prev := int32(1)
	if r := c.srcSet.Spec.Replicas; r != nil {
		prev = *r
	}
	if err := c.jj.Update(func(j *Journal) {
		j.Source.Stopped, j.Source.PrevReplicas = true, &prev
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	backend.ReportStep(ctx, fleet.StepSeed, "stopping the source for the final copy (clone_consistency stop-source)")
	if err := c.sourceHandle().Stop(c.srcID); err != nil {
		return fmt.Errorf("stopping the source for the final copy: %w", err)
	}
	return nil
}

// sourceHandle is a handle on the source instance.
func (c *cloning) sourceHandle() *Backend {
	r := c.srcRec
	return newHandle(&fleet.Instance{
		Name: r.Instance, KubeCluster: r.Cluster, KubeKey: r.Key, ContainerID: c.srcID, CreatedAt: r.Created,
		WorkspaceDir: filepath.Join(state.WorkspacesDir(), r.Fleet, r.Instance, r.Fleet), Backend: fleet.BackendKubernetes,
	}, c.p)
}

// letGo ends the hold on the source: a frozen one is thawed (and the clone
// best-effort when the watchdog thawed it first), a stopped one started
// again, which the clone waits for before it ends (waitRestart).
func (c *cloning) letGo() {
	c.mu.Lock()
	frozen, stopped := c.frozen, c.stopped && c.restarting == nil
	c.frozen = false
	if stopped {
		c.restarting = make(chan error, 1)
	}
	restarting := c.restarting
	c.mu.Unlock()
	if frozen {
		ctx, cancel := context.WithTimeout(c.p.ctx, 15*time.Second)
		defer cancel()
		var th podwire.ThawInfo
		sess, err := c.sourceSession(ctx)
		if err == nil {
			err = sess.Client().Call(ctx, podwire.MethodThaw, nil, &th)
		}
		switch {
		case err != nil:
			flog.Warn("kube clone: thawing the source; its watchdog thaws it", "fleet", c.id.fleet, "src", c.src.sts, "err", err)
			c.markBestEffort(context.Background(), "the source's thaw could not be confirmed, so the copy may have outlasted its freeze")
		case th.ByWatchdog:
			c.markBestEffort(context.Background(), fmt.Sprintf("the copy outlasted the source's freeze (%s), so the source ran on before it ended", cloneFreeze))
		default:
			flog.Info("kube clone: thawed the source", "fleet", c.id.fleet, "src", c.src.sts)
		}
	}
	if stopped {
		go func() {
			err := c.sourceHandle().Start(c.srcID)
			if err == nil {
				_ = c.jj.Update(func(j *Journal) { j.Source.Stopped = false })
			}
			restarting <- err
		}()
	}
}

// waitRestart waits for a source the clone stopped to be running again.
func (c *cloning) waitRestart() error {
	c.mu.Lock()
	ch := c.restarting
	c.mu.Unlock()
	if ch == nil {
		return nil
	}
	err := <-ch
	ch <- err
	return err
}

// markBestEffort marks the clone best-effort, with why, once: its banner
// says so.
func (c *cloning) markBestEffort(ctx context.Context, why string) {
	c.mu.Lock()
	first := c.bestEffort == ""
	if first {
		c.bestEffort = why
	}
	c.mu.Unlock()
	if first {
		c.warn(ctx, "best-effort clone: "+why)
	}
}

// --- the clone's transient objects ---

// applyTransient applies a body of one of the clone's own passing objects
// (its snapshot), journaled as the clone's.
func (c *cloning) applyTransient(ctx context.Context, kind, name string, body []byte) error {
	if err := c.jj.Intend(JournalObject{Kind: kind, Name: name, Gen: c.gen, Created: true}); err != nil {
		return err
	}
	uid, err := applyObject(ctx, c.cs, c.ns, kind, name, body)
	if err != nil {
		return classify(fmt.Errorf("applying %s: %w", objectRef(kind, name), err))
	}
	if err := c.jj.Applied(kind, name, uid); err != nil {
		return err
	}
	c.mu.Lock()
	c.transient = append(c.transient, JournalObject{Kind: kind, Name: name, UID: uid})
	c.mu.Unlock()
	return nil
}

func (c *cloning) applyTransientObject(ctx context.Context, kind, name string, obj metav1.Object) error {
	body, err := podspec.ApplyBody(obj)
	if err != nil {
		return err
	}
	return c.applyTransient(ctx, kind, name, body)
}

// deleteTransient deletes the clone's copier Jobs and snapshots: the clone's
// disk no longer needs them.
func (c *cloning) deleteTransient() {
	c.mu.Lock()
	objs := c.transient
	c.transient = nil
	c.mu.Unlock()
	if len(objs) == 0 {
		return
	}
	ctx, cancel := c.cleanupContext()
	defer cancel()
	s, _ := c.scope()
	var metas []objectMeta
	for _, kind := range teardownKinds {
		for _, o := range objs {
			if o.Kind == kind {
				metas = append(metas, objectMeta{Kind: o.Kind, Name: o.Name, UID: o.UID})
			}
		}
	}
	if _, err := s.deleteAll(ctx, metas); err != nil {
		flog.Warn("kube clone: deleting its copiers and snapshots", "fleet", c.id.fleet, "instance", c.id.name, "err", err)
	}
}
