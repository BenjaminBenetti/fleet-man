//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// cloneFreezeRenew is how often fleetd renews the freeze while it holds
	// the source, and cloneHoldMax the longest it holds a source frozen: a
	// snapshot not cut by then (a driver that copies the disk to cut it) or a
	// final delta not done is of the running source, and the clone
	// best-effort.
	cloneFreezeRenew = 10 * time.Second
	cloneHoldMax     = 2 * time.Minute
)

// freezeBounds are a freeze's bounds as it began (cloneFreeze,
// cloneFreezeRenew and cloneHoldMax): its renewals go by them.
type freezeBounds struct {
	watchdog, renew, max time.Duration
}

// copierResources are a copier's requests: it runs beside the running
// source, on its node, and asks for little.
func copierResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}}
}

// The source disk's mount path in a copier.
const cloneSourcePath = "/fleet-src"

// EnvCloneHoldPause holds a clone's source this long once it is held still
// (frozen or stopped) before the copy goes on: a knob for tests of a daemon
// that dies while it holds a source (the pod's watchdog then thaws it).
// Unset, the hold lasts as long as the copy does.
const EnvCloneHoldPause = "FLEET_KUBE_CLONE_HOLD_PAUSE"

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
		"best_effort", c.bestEffortWhy() != "", "container", res.ContainerID, "ms", time.Since(c.start).Milliseconds())
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
	driver  string
	plan    ClonePlan

	// jobCtx is the clone job's (its log).
	jobCtx context.Context
	// held is the hold on the source in force; bestEffort, when set, says
	// why the clone is not of one point in time. holdMu orders the freeze,
	// its renewals and the thaw.
	holdMu     sync.Mutex
	mu         sync.Mutex
	renewStop  chan struct{}
	frozen     bool
	stopped    bool
	restarting chan error
	bestEffort string
	// slot is the source's hold slot (sourceHolds) while the clone holds
	// the source still; bounds are its freeze's.
	slot   chan struct{}
	bounds freezeBounds
	// transient are the clone's own objects that go once it is done (its
	// snapshot, its copier Job), by kind.
	transient []JournalObject
}

func (c *cloning) run(ctx context.Context) (*backend.UpResult, error) {
	c.jobCtx = ctx
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	// The clone's pod mounts the fleet's share as its source's does: a
	// move of it is called off or waited for, and held off until the
	// clone ends.
	if err := c.shareMoveGate(ctx); err != nil {
		return nil, err
	}
	defer c.releaseMoveHolds()
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
	res.Warnings = c.cloneWarnings()
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
	// A source another clone holds still is read once it is let go: one
	// stopped for another clone's final copy is no stopped source.
	slot, err := c.takeSlot(ctx, fleet.StepBackendClone)
	if err != nil {
		return err
	}
	<-slot
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
	known := storageClassOf(c.facts.Capabilities, class)
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
	f.CSIClone = f.CSIClone || podspec.ClonesVolumes(f.Driver)
	f.SnapshotClass = snapshotClassOf(ctx, c.cs, f.Driver, c.rec.SnapshotClass)
	if _, refused := refusedDrivers.Load(c.id.cluster + "|" + f.Driver); refused && (f.Method == "" || f.Method == fleet.KubeCloneAuto) {
		f.CSIClone, f.SnapshotClass = false, "" // it refused this run: copy at once
	}
	c.driver = f.Driver
	return f, nil
}

// storageClassOf is the preflight's finding for StorageClass class ("" is
// the cluster's default), or nil when it found none.
func storageClassOf(caps *fleetgrpc.KubeCapabilities, class string) *fleetgrpc.KubeStorageClass {
	var known *fleetgrpc.KubeStorageClass
	for _, sc := range caps.GetStorageClasses() {
		if sc.GetName() == class || (class == "" && sc.GetIsDefault()) {
			known = sc
		}
	}
	return known
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
		if err := c.hold(ctx, fleet.StepBackendClone); err != nil {
			return nil, err
		}
		source = pvcSource(c.srcDisk.Name)
	}

	if err := c.step(ctx, stepApply, fleet.StepApplyManifests, ""); err != nil {
		return nil, err
	}
	o, err := c.applyDisk(ctx, source, size)
	if err != nil {
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
	err = c.copyDisk(ctx, o)
	var refused *diskRefusedError
	if errors.As(err, &refused) && c.plan.Tier != TierCopy && (c.rec.CloneMethod == "" || c.rec.CloneMethod == fleet.KubeCloneAuto) {
		// The driver did not make the disk from the source's: it is copied
		// instead, and the next clones on the connection copy at once.
		o, err = c.fallBackToCopy(ctx, refused)
		if err == nil {
			err = c.copyDisk(ctx, o)
		}
	}
	if err != nil {
		return nil, err
	}
	c.letGo()

	if err := c.step(ctx, stepStart, fleet.StepWaitPod, ""); err != nil {
		return nil, err
	}
	return c.startClone(ctx, o)
}

// applyDisk renders the clone's objects, its disk made from source (nil: an
// empty disk a copier fills), and applies the disk.
func (c *cloning) applyDisk(ctx context.Context, source *corev1.TypedLocalObjectReference, size string) (*podspec.Objects, error) {
	o, err := podspec.CloneObjects(podspec.CloneSource{StatefulSet: c.srcSet, SpecSecret: c.srcSpec, EnvSecret: c.srcEnv, Disk: c.srcDisk, Gen: c.gen}, c.cloneInput(source, size))
	if err != nil {
		return nil, err
	}
	if err := c.apply(ctx, KindPVC, o.Disk.Name, true, o.Disk); err != nil {
		return nil, err
	}
	return o, nil
}

// fallBackToCopy gives up on a disk the driver would not restore or clone:
// the disk and the Job waiting on it go, and an empty disk is made for a
// copier, which the source is held still for as the plan says.
func (c *cloning) fallBackToCopy(ctx context.Context, refused *diskRefusedError) (*podspec.Objects, error) {
	flog.Warn("kube clone: the driver did not make the clone's disk; copying it instead", "fleet", c.id.fleet, "instance", c.id.name, "tier", c.plan.Tier, "driver", refused.driver, "err", refused)
	backend.ReportWarning(ctx, fmt.Sprintf("%s; copying the source's disk instead", refused.Error()))
	refusedDrivers.Store(c.id.cluster+"|"+refused.driver, true)
	c.letGo()
	c.deleteTransient()
	ctx2, cancel := c.cleanupContext()
	defer cancel()
	s, _ := c.scope()
	if _, err := s.deleteAll(ctx2, []objectMeta{{Kind: KindPVC, Name: c.id.sts}}); err != nil {
		return nil, fmt.Errorf("deleting the clone's refused disk: %w", err)
	}
	for {
		if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, c.id.sts, metav1.GetOptions{}); gone(err) {
			break
		} else if err != nil {
			return nil, classify(err)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return nil, fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
	}
	// A source stopped for the restore is running again before the copier
	// holds it (or stops it) anew.
	if err := c.waitRestart(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.stopped, c.restarting = false, nil
	c.mu.Unlock()
	c.plan = ClonePlan{Tier: TierCopy, Hold: c.plan.Hold, Why: "the driver did not make the disk from the source's"}
	backend.ReportStep(ctx, fleet.StepSeed, "copy (the driver did not make the disk from the source's)")
	return c.applyDisk(ctx, nil, "")
}

// refusedDrivers are the connections' drivers that refused to make a
// clone's disk from its source's this run (cluster|driver): later clones
// there copy at once.
var refusedDrivers sync.Map

// diskRefusedError is a clone's disk its driver would not make from the
// source's.
type diskRefusedError struct {
	tier   CloneTier
	driver string
	why    string
}

func (e *diskRefusedError) Error() string {
	return fmt.Sprintf("%s: %s did not make the clone's disk from the source's (%s): %s", kubeerr.Misconfigured, e.driver, e.tier, e.why)
}

// Code is the error's kubernetes: code.
func (e *diskRefusedError) Code() string { return string(kubeerr.Misconfigured) }

// cloneInput is who the clone is, for its objects.
func (c *cloning) cloneInput(source *corev1.TypedLocalObjectReference, size string) podspec.CloneInput {
	rec := c.rec
	return podspec.CloneInput{
		Cluster: &rec, Namespace: c.ns, Name: c.id.sts, Key: c.id.key, Owner: c.owner, ClusterID: c.id.cluster,
		FleetName: c.id.fleet, FleetHash: names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet), InstanceName: c.id.name,
		WorkspaceDir: c.req.WorkspaceDir, Replicas: 1, DiskSource: source, DiskSize: size,
	}
}

// snapshotErrorGrace is how long a snapshot may report an error, while its
// controller retries, before the clone gives up on it. A var for tests.
var snapshotErrorGrace = 5 * time.Minute

// snapshotErrors follows a snapshot's reported error. The error is the last
// its controller saw, and the controller retries (a driver's own timeout
// clears on the next try), so only one that lasts snapshotErrorGrace fails.
type snapshotErrors struct{ since time.Time }

// see takes the snapshot's error msg (or none) at now: first is an error
// newly reported, fails one that has lasted the grace. A ready snapshot
// has no error that counts.
func (e *snapshotErrors) see(msg string, ready bool, now time.Time) (first, fails bool) {
	switch {
	case msg == "" || ready:
		e.since = time.Time{}
		return false, false
	case e.since.IsZero():
		e.since = now
		return true, false
	}
	return false, now.Sub(e.since) >= snapshotErrorGrace
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
	if err := c.hold(ctx, fleet.StepBackendClone); err != nil {
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
	var failing snapshotErrors
	for {
		v, err := readSnapshot(ctx, c.cs, c.ns, name)
		if err != nil && !gone(err) {
			return "", "", classify(fmt.Errorf("reading the source's snapshot %s: %w", name, err))
		}
		var st *snapshotStatus
		if v != nil {
			st = v.Status
		}
		msg := st.failure()
		switch first, fails := failing.see(msg, st.ready(), time.Now()); {
		case fails:
			return "", "", fmt.Errorf("the snapshot of the source's disk failed: %s", msg)
		case first:
			flog.Warn("kube clone: the snapshot of the source's disk reports an error; its controller retries", "fleet", c.id.fleet, "snapshot", name, "err", msg)
		}
		if st.cut() || st.ready() {
			c.letGo()
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
			// Where the source runs now (it may have been started again
			// since it was read).
			if pod, err := c.p.Verify(ctx, c.srcRec.target()); err == nil {
				c.srcNode = pod.Spec.NodeName
			}
			// Beside the running source: its disk is attached to its node.
			job.Node, job.Resources = c.srcNode, copierResources()
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
	img := c.srcRec.expect()
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
			if st := containerStatus(pod, podspec.JobContainer); st != nil && st.ImageID != "" && !img.runsImage(st) {
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
			return &diskRefusedError{tier: c.plan.Tier, driver: c.driver, why: "the disk came up empty"}
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

// diskRefused is a copier Job's Pending check: a disk to restore from a
// snapshot or clone that the provisioner keeps refusing (a driver that cannot
// restore or clone it, or whose copy outlasts the provisioner's deadline)
// fails at once, naming why, rather than waiting out the disk's deadline.
func (c *cloning) diskRefused(pvc string) func(context.Context) error {
	if c.plan.Tier == TierCopy {
		return nil
	}
	driver := c.driver
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
			outright := strings.Contains(msg, "data source") || strings.Contains(msg, "datasource") || strings.Contains(msg, "not support") || strings.Contains(msg, "unimplemented")
			if outright || ev.Count >= 3 {
				return &diskRefusedError{tier: c.plan.Tier, driver: driver, why: ev.Message}
			}
		}
		return nil
	}
}

// cloneRecord is fleetd's record of the clone's pod: the source's (its
// image, user, creation time, published ports, host relays and whether it
// lacks the audio stack: its pod template is the source's), under the
// clone's name, key and spec hash.
// StatefulSetUID comes once the StatefulSet is applied.
func (c *cloning) cloneRecord(o *podspec.Objects) InstanceRecord {
	src := c.srcRec
	return InstanceRecord{
		Cluster: c.id.cluster, Namespace: c.ns, Key: c.id.key, Fleet: c.id.fleet, Instance: c.id.name,
		StatefulSet: c.id.sts, Gen: c.gen, SpecHash: podspec.SpecHash(&o.StatefulSet.Spec.Template.Spec),
		Image: src.Image, ImageDigests: slices.Clone(src.ImageDigests), ImageConfig: src.ImageConfig, Strategy: src.Strategy, RemoteUser: src.RemoteUser,
		WorkspaceFolder: src.WorkspaceFolder, NonRoot: src.NonRoot, AudioMissing: src.AudioMissing, Created: src.Created, Ports: slices.Clone(src.Ports),
		HostSockets: slices.Clone(src.HostSockets), HostSocketDirs: cloneSocketDirs(src.HostSocketDirs), HostGateway: src.HostGateway, GatewaySkip: slices.Clone(src.GatewaySkip),
		Share: src.Share.clone(), DockerImage: dockerImageOf(o),
	}
}

// startClone starts the clone's pod and waits for it, verified, with its
// agent.
func (c *cloning) startClone(ctx context.Context, o *podspec.Objects) (*backend.UpResult, error) {
	src := c.srcRec
	rec := c.cloneRecord(o)
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
	if st := containerStatus(pod, DevContainer); st != nil && st.ImageID != "" && !rec.expect().runsImage(st) {
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
		return c.hold(ctx, fleet.StepSeed)
	case podseed.BarrierCopied:
		flog.Info("kube clone: copied", "fleet", c.id.fleet, "instance", c.id.name, "detail", b.Detail)
		c.letGo()
		return nil
	}
	return fmt.Errorf("a clone's copier has no barrier %q", b.Name)
}

// hold holds the running source still as the plan says: its user processes
// frozen (a pod that cannot be is copied live, and the clone is
// best-effort), or the source stopped. A stopped source needs nothing, and a
// source the clone holds already (a barrier sent again on a newer
// connection) is held once. js is the job's step, for a wait on another
// clone's hold of the source.
func (c *cloning) hold(ctx context.Context, js fleet.JobStep) error {
	if !c.running || c.isHeld() {
		return nil
	}
	slot, err := c.takeSlot(ctx, js)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.slot = slot
	c.mu.Unlock()
	switch c.plan.Hold {
	case HoldFreeze:
		c.freeze(ctx)
	case HoldStopSource:
		err = c.stopSource(ctx)
	}
	if !c.isHeld() {
		// Nothing held (a pod that cannot be frozen, a stop that never
		// began): another clone may hold the source.
		c.releaseSlot()
	}
	if err != nil {
		return err
	}
	if pause, ok := parseTimeout(os.Getenv(EnvCloneHoldPause)); ok && c.isHeld() {
		flog.Info("kube clone: holding the source ("+EnvCloneHoldPause+")", "fleet", c.id.fleet, "src", c.src.sts, "pause", pause.String())
		if err := sleepCtx(ctx, pause); err != nil {
			return fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
	}
	return nil
}

// isHeld reports whether the clone holds the source still now: frozen, or
// stopped and not yet being started again.
func (c *cloning) isHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frozen || (c.stopped && c.restarting == nil)
}

// sourceHolds are the slots of the sources this daemon's clones hold still,
// one hold at a time per source (cluster|StatefulSet → a one-place
// channel): one clone's thaw or restart of a source never ends another
// clone's hold of it, and no clone reads a source another one stopped for
// its final copy as a stopped source.
var sourceHolds sync.Map

// takeSlot waits until no other clone holds the source still, and takes the
// source's slot; js is the step a wait is reported under.
func (c *cloning) takeSlot(ctx context.Context, js fleet.JobStep) (chan struct{}, error) {
	v, _ := sourceHolds.LoadOrStore(c.id.cluster+"|"+c.src.sts, make(chan struct{}, 1))
	slot := v.(chan struct{})
	select {
	case slot <- struct{}{}:
		return slot, nil
	default:
	}
	backend.ReportStep(ctx, js, "waiting for another clone of the source to let it go")
	select {
	case slot <- struct{}{}:
		return slot, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
}

// releaseSlot gives up the source's slot the clone took for a hold that
// never came to be.
func (c *cloning) releaseSlot() {
	c.mu.Lock()
	slot := c.slot
	c.slot = nil
	c.mu.Unlock()
	if slot != nil {
		<-slot
	}
}

// sourceSession is the running source's agent session.
func (c *cloning) sourceSession(ctx context.Context) (*AgentSession, error) {
	t := c.srcRec.target()
	t.Priority = PriorityInteractive
	s, err := c.p.EnsureAgent(ctx, t)
	return s, classify(err)
}

// freeze freezes the source's user processes; where it cannot, the clone
// goes on live: a copier's is best-effort (its final delta runs live), a
// snapshot's is not (the snapshot of a live disk is of one point in time,
// as a crash leaves it; snapshot flushes the disk first). The freeze's
// watchdog thaws the source cloneFreeze after fleetd last renewed it, which
// fleetd does while the hold lasts, for at most cloneHoldMax: a fleetd that
// dies leaves the source frozen no longer than the watchdog.
func (c *cloning) freeze(ctx context.Context) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	bounds := freezeBounds{watchdog: cloneFreeze, renew: cloneFreezeRenew, max: cloneHoldMax}
	sess, err := c.sourceSession(ctx)
	if err == nil {
		var info podwire.FreezeInfo
		err = sess.Client().Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: bounds.watchdog.Milliseconds()}, &info)
		if err == nil {
			stop := make(chan struct{})
			c.mu.Lock()
			c.frozen, c.renewStop, c.bounds = true, stop, bounds
			c.mu.Unlock()
			flog.Info("kube clone: froze the source", "fleet", c.id.fleet, "src", c.src.sts, "settle_ms", info.SettleMS, "sync_ms", info.SyncMS)
			go c.renewFreeze(stop, time.Now(), bounds)
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
	if c.plan.Tier != TierCopy {
		flog.Info("kube clone: the source runs on for its snapshot ("+why+")", "fleet", c.id.fleet, "src", c.src.sts)
		return
	}
	c.markBestEffort("the source ran on while it was copied (" + why + "); files written meanwhile may differ (set clone_consistency to stop-source to stop it instead)")
}

// renewFreeze renews the source's freeze every b.renew until stop (the
// freeze it renews is let go), and lets the source go once it has been
// frozen b.max. A renewal never freezes the source again: a freeze its
// watchdog ended meanwhile (fleetd's renewals were late) makes the clone
// best-effort.
func (c *cloning) renewFreeze(stop chan struct{}, since time.Time, b freezeBounds) {
	t := time.NewTicker(b.renew)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c.holdMu.Lock()
		c.mu.Lock()
		current := c.frozen && c.renewStop == stop
		c.mu.Unlock()
		if !current {
			c.holdMu.Unlock()
			return
		}
		if time.Since(since) >= b.max {
			c.holdMu.Unlock()
			c.markBestEffort(fmt.Sprintf("the copy outlasted the longest fleet holds a source frozen (%s), so the source ran on before it ended", b.max))
			c.letGo()
			return
		}
		ctx, cancel := context.WithTimeout(c.p.ctx, b.renew)
		sess, err := c.sourceSession(ctx)
		if err == nil {
			err = sess.Client().Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: b.watchdog.Milliseconds(), Renew: true}, nil)
		}
		cancel()
		c.holdMu.Unlock()
		switch {
		case podwire.IsCode(err, podwire.CodeThawed):
			// The thaw that lets the source go clears the freeze's record.
			c.markBestEffort(fmt.Sprintf("the source's freeze ended (its %s watchdog) before fleet renewed it, so the source ran on before the copy ended", b.watchdog))
			return
		case err != nil:
			flog.Warn("kube clone: renewing the source's freeze; its watchdog thaws it", "fleet", c.id.fleet, "src", c.src.sts, "err", err)
		}
	}
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
// best-effort when something thawed it first), a stopped one started again,
// which the clone waits for before it ends (waitRestart). The source's slot
// goes once it is let go: after the thaw, or once it runs again.
func (c *cloning) letGo() {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.mu.Lock()
	frozen, stopped := c.frozen, c.stopped && c.restarting == nil
	c.frozen = false
	if c.renewStop != nil {
		close(c.renewStop)
		c.renewStop = nil
	}
	if stopped {
		c.restarting = make(chan error, 1)
	}
	restarting, slot, bounds := c.restarting, c.slot, c.bounds
	c.slot = nil
	c.mu.Unlock()
	release := func() {
		if slot != nil {
			<-slot
		}
	}
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
			c.markBestEffort("the source's thaw could not be confirmed, so the copy may have outlasted its freeze")
		case th.ByWatchdog:
			c.markBestEffort(fmt.Sprintf("the source's freeze ended (its %s watchdog) before the copy did, so the source ran on before it ended", bounds.watchdog))
		case !th.WasFrozen:
			c.markBestEffort("the source was no longer frozen when the copy ended (it was thawed or restarted meanwhile), so it ran on before the copy ended")
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
			release()
			restarting <- err
		}()
		return
	}
	release()
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

// markBestEffort marks the clone best-effort, with why, once: the job's log
// says so at once, its banner at the end (cloneWarnings). It may run beside
// the clone (the freeze's renewals).
func (c *cloning) markBestEffort(why string) {
	c.mu.Lock()
	first := c.bestEffort == ""
	if first {
		c.bestEffort = why
	}
	ctx := c.jobCtx
	c.mu.Unlock()
	if first {
		backend.ReportWarning(ctx, bestEffortPrefix+why)
	}
}

// bestEffortPrefix opens a best-effort clone's banner line.
const bestEffortPrefix = "best-effort clone: "

// bestEffortWhy is why the clone is best-effort, "" when it is not.
func (c *cloning) bestEffortWhy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bestEffort
}

// cloneWarnings are the clone's banner lines: that it is best-effort first,
// when it is, then the create's warnings.
func (c *cloning) cloneWarnings() []string {
	lines := c.warningLines()
	if why := c.bestEffortWhy(); why != "" {
		lines = append([]string{bestEffortPrefix + why}, lines...)
	}
	return lines
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
