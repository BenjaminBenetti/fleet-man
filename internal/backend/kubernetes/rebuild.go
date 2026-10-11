//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// rebuild.go is an instance's rebuild on its cluster
// (backend.RebuildContexter): a new root generation from the devcontainer as
// the instance has it now, the workspace and volumes kept, the ContainerID's
// generation bumped.
//
//  1. The workspace comes back to the daemon host's checkout from wherever
//     the instance is (rebuild_export): a running pod sends it over its bulk
//     session, a stopped one's disk is read by a tool Job. The image builds
//     from that checkout, as docker's rebuild builds from the bind-mounted
//     one, so a devcontainer.json edited in the pod is the one built.
//  2. The image is built and delivered, held apart from the one the instance
//     runs until the rebuild succeeds, and the disk is checked for room for
//     the new root plus 10% (grown where the class allows, with
//     auto_expand_disk on). A rebuild that fails up to here changed nothing.
//  3. From the journal on: the pod is stopped; the next generation's Secrets
//     and seed Job are applied, and the rollback Job and the current
//     StatefulSet recorded; the seed resets the root (the current one goes to
//     trash) and copies the new image; the StatefulSet is re-rendered over
//     the new root and started, verified, given its agent; the full
//     lifecycle hooks run, as for a new container.
//  4. Success ends the journal, then deletes the trash and the previous
//     generation's Secrets, and the new image takes over the old one's hold.
//     A failure is undone (Pool.UndoRebuild): the previous root back from
//     trash, its StatefulSet re-applied with its replicas, fleet's record of
//     its pod restored, and the rebuild fails kubernetes:rebuild_failed with
//     that generation in place; another rebuild is the retry.
//
// An instance that never reached running (an interrupted or failed create,
// which has no ContainerID) has nothing to export or keep: its rebuild is the
// retry of its create. Whatever the create left is deleted by key, and the
// create runs again from the daemon host's checkout.

// The steps a rebuild journals besides the create's (stepStart, stepAgent,
// stepHooks) and StepReset.
const (
	stepPrepare Step = "prepare"
	stepStop    Step = "stop"
)

// Bounds of a rebuild's own work (its waits have their own deadlines).
const (
	// exportHold bounds a stopped instance's workspace export: the tool
	// Job holds its pod this long at most.
	exportHold = 2 * time.Hour
	// undoTimeout bounds undoing a failed rebuild in-process.
	undoTimeout = 15 * time.Minute
	// commitTimeout bounds deleting a rebuilt instance's trash.
	commitTimeout = 30 * time.Minute
	// keyGoneTimeout bounds the wait for a failed create's objects to be
	// gone before it is created again.
	keyGoneTimeout = 3 * time.Minute
)

// rebuildHeadroom is the room a rebuild needs on the instance disk beyond the
// new root's size: 10% of it.
const rebuildHeadroom = 10

// The rebuild_export values (KubeCluster.RebuildExport).
const (
	// RebuildExportAuto ("" too) exports the workspace from wherever the
	// instance is before it builds.
	RebuildExportAuto = "auto"
	// RebuildExportOff builds from the daemon host's checkout as it is.
	RebuildExportOff = "off"
)

// RebuildContext rebuilds the instance (backend.RebuildContexter): see
// rebuild.go. ctx is the rebuild job's: cancelling it (a destroy, the daemon
// shutting down) stops the rebuild at its next step; a destroy then deletes
// everything, and a shutdown leaves the journal for the next start to undo.
func (b *Backend) RebuildContext(ctx context.Context, containerID string, req backend.UpRequest) (*backend.UpResult, error) {
	id, err := b.identity()
	if err != nil {
		return nil, err
	}
	if containerID == "" {
		containerID = b.inst.ContainerID
	}
	start := time.Now()
	var res *backend.UpResult
	if containerID == "" {
		res, err = b.retryCreate(ctx, id, req)
	} else {
		rb := &rebuilding{creation: newCreation(b, id, req), oldID: containerID}
		res, err = rb.run(ctx)
	}
	if err != nil {
		flog.Error("kube rebuild failed", "fleet", id.fleet, "instance", id.name, "cluster", id.cluster, "key", id.key, "ms", time.Since(start).Milliseconds(), "err", err)
		return nil, err
	}
	flog.Info("kube rebuild: instance running", "fleet", id.fleet, "instance", id.name, "cluster", id.cluster, "container", res.ContainerID, "ms", time.Since(start).Milliseconds())
	return res, nil
}

// Rebuild is RebuildContext without a job's context.
func (b *Backend) Rebuild(containerID, workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return b.RebuildContext(context.Background(), containerID, backend.UpRequest{WorkspaceDir: workspaceDir, Mounts: mounts})
}

// retryCreate is the rebuild of an instance that never reached running: the
// retry of its create. What the create left (by key: a seeded disk, Secrets,
// Jobs, a StatefulSet) is deleted and waited out, fleetd's record of its pod
// dropped, and the create runs again under the same key.
func (b *Backend) retryCreate(ctx context.Context, id bound, req backend.UpRequest) (*backend.UpResult, error) {
	p := b.p()
	backend.ReportStep(ctx, fleet.StepBackendDown, "deleting what the failed create left")
	td, err := p.DeleteByKey(ctx, id.cluster, id.key)
	if err != nil {
		return nil, classify(fmt.Errorf("deleting what the failed create left: %w", err))
	}
	if err := p.waitKeyGone(ctx, id.cluster, id.key); err != nil {
		return nil, classify(err)
	}
	p.Forget(PodTarget{Cluster: id.cluster, StatefulSet: id.sts})
	if err := RemoveInstanceRecord(id.cluster, id.key); err != nil {
		return nil, err
	}
	flog.Info("kube rebuild: retrying a create that never ran; its objects are deleted", "fleet", id.fleet, "instance", id.name, "cluster", id.cluster, "key", id.key, "deleted", len(td.Deleted))
	return b.UpContext(ctx, req)
}

// waitKeyGone waits until nothing of instance key is left on the connection
// (foreground deletions take their time), bounded by keyGoneTimeout.
func (p *Pool) waitKeyGone(ctx context.Context, clusterID, key string) error {
	s, err := p.Scope(clusterID)
	if err != nil {
		return err
	}
	deadline := p.opts.Now().Add(keyGoneTimeout)
	for {
		left, err := s.ListByKey(ctx, key)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		if p.opts.Now().After(deadline) {
			return &DeadlineError{Kind: CodeTimeout, Wait: fmt.Sprintf("the failed create's %d objects to be deleted", len(left)), After: keyGoneTimeout}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}

// rebuilding is one rebuild in progress: the creation of generation gen
// over the instance's disk, with what it found of generation prevGen.
type rebuilding struct {
	*creation
	oldID string

	// prev is fleetd's record of the instance's pod at prevGen, sts its
	// StatefulSet as the rebuild found it, replicas its replicas then.
	prev     InstanceRecord
	sts      *appsv1.StatefulSet
	replicas int32
	// sess is the running pod's agent session (the export, the disk check).
	sess *AgentSession
	// disk is the instance disk's room, as a stopped instance's tool Job
	// found it; nil until then.
	disk *podseed.DiskSpace
	// delivered: the new image is held by holder, which a rebuild that
	// does not succeed lets go of.
	delivered bool
}

func (rb *rebuilding) running() bool { return rb.replicas > 0 }

// run is the rebuild of an instance that has a generation running or
// stopped.
func (rb *rebuilding) run(ctx context.Context) (*backend.UpResult, error) {
	if err := rb.connect(ctx); err != nil {
		return nil, rb.unchanged(err)
	}
	if err := rb.prepare(ctx); err != nil {
		return nil, rb.unchanged(err)
	}
	// The share may be prepared only once the run is translated (admit).
	defer func() { rb.share.done() }()
	defer rb.releaseVolumes()
	if err := rb.load(ctx); err != nil {
		return nil, rb.unchanged(err)
	}
	if err := rb.export(ctx); err != nil {
		return nil, rb.unchanged(err)
	}
	cfg, err := devcli.ReadMerged(ctx, devcli.ReadRequest{Tools: rb.env.Tools, WorkspaceDir: rb.req.WorkspaceDir})
	if err != nil {
		return nil, rb.unchanged(kubeerr.Wrap(kubeerr.ImageBuild, err))
	}
	if err := rb.preflight(ctx, cfg); err != nil && !rb.quotaWhileRunning(err) {
		return nil, rb.unchanged(err)
	}
	img, err := rb.build(ctx, cfg)
	if err != nil {
		return nil, rb.unchanged(err)
	}
	rb.delivered = true
	if err := rb.checkDisk(ctx, img); err != nil {
		return nil, rb.unchanged(err)
	}
	if err := rb.step(ctx, stepAdmit, fleet.StepApplyManifests, "admission"); err != nil {
		return nil, rb.unchanged(err)
	}
	adm, r, err := rb.admit(ctx, img, cfg)
	// A quota the running pod fills (a pod count, the compute it holds)
	// is judged again once that pod is stopped.
	deferred := err != nil && rb.quotaWhileRunning(err)
	if err != nil && !deferred {
		return nil, rb.unchanged(err)
	}

	if err := rb.begin(ctx); err != nil {
		return nil, rb.unchanged(err)
	}
	res, err := rb.reseed(ctx, adm, r, img, cfg, deferred)
	if err != nil {
		return nil, rb.failed(ctx, err)
	}
	res.Warnings = rb.warningLines()
	rb.commit(adm, r, res)
	return res, nil
}

// quotaWhileRunning reports whether err is a quota refusal a running pod of
// the instance may be the cause of.
func (rb *rebuilding) quotaWhileRunning(err error) bool {
	code, ok := kubeerr.Of(err)
	return ok && code.Is(kubeerr.Quota) && rb.running()
}

// load reads what the rebuild starts from: fleetd's record of the pod, a
// kept journal's undo finished, the StatefulSet (verified as fleet's).
func (rb *rebuilding) load(ctx context.Context) error {
	prev, err := LoadInstanceRecord(rb.id.cluster, rb.id.key)
	if err != nil {
		return fmt.Errorf("fleet's record of the instance's pod: %w (destroy the instance and create it again)", err)
	}
	if prev.StatefulSet != rb.id.sts {
		return fmt.Errorf("fleet's record of the instance names StatefulSet %s, not %s", prev.StatefulSet, rb.id.sts)
	}
	if !names.ValidGeneration(prev.Gen + 1) {
		return fmt.Errorf("the instance is at root generation %d, the last one fleet makes", prev.Gen)
	}
	if prev.NonRoot != rb.identity.NonRoot {
		return kubeerr.New(kubeerr.AdmissionDenied, "the namespace's admission changed since the instance was created (its pods ran %s, they would now run %s): rebuilding would change the owners on its disk; destroy the instance and create it again", rootWords(prev.NonRoot), rootWords(rb.identity.NonRoot))
	}
	rb.prev = prev
	rb.gen, rb.prevGen, rb.created = prev.Gen+1, prev.Gen, time.Now().UTC()
	rb.holder = rebuildHolder(rb.id.key, rb.gen)

	if err := rb.finishKeptUndo(ctx); err != nil {
		return err
	}
	sts, err := rb.cs.AppsV1().StatefulSets(rb.ns).Get(ctx, prev.StatefulSet, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the instance's StatefulSet: %w", err))
	}
	var reasons []string
	if string(sts.UID) != prev.StatefulSetUID {
		reasons = append(reasons, "it was made again since fleet made it (another UID)")
	}
	if podspec.SpecHash(&sts.Spec.Template.Spec) != prev.SpecHash {
		reasons = append(reasons, "its pod template differs from fleet's render")
	}
	if len(reasons) > 0 {
		return kubeerr.New(kubeerr.Tampered, "StatefulSet %s/%s is not the one fleet made: %s", rb.ns, sts.Name, strings.Join(reasons, "; "))
	}
	rb.sts = sts
	if sts.Spec.Replicas != nil {
		rb.replicas = *sts.Spec.Replicas
	}
	return nil
}

// prepare learns the class of the disk the rebuild keeps, then readies the
// fleet's share: the share's checks of the disk's binding and the render
// describe that disk as it is, whatever Auto would pick for a new one now.
func (rb *rebuilding) prepare(ctx context.Context) error {
	if disk, err := rb.cs.CoreV1().PersistentVolumeClaims(rb.ns).Get(ctx, rb.id.sts, metav1.GetOptions{}); err == nil && disk.Spec.StorageClassName != nil {
		rb.keptDiskClass = *disk.Spec.StorageClassName
	}
	return rb.prepareShare(ctx)
}

func rootWords(nonRoot bool) string {
	if nonRoot {
		return "without root"
	}
	return "as root"
}

// finishKeptUndo finishes the undo of an earlier rebuild of the instance that
// failed and kept its journal (its undo failed too): a retry starts from the
// generation that rebuild replaced.
func (rb *rebuilding) finishKeptUndo(ctx context.Context) error {
	j, err := ReadJournal(JournalPath(rb.id.key))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("the instance's job journal: %w", err)
	case j.Cluster != rb.id.cluster || j.Instance != rb.id.name:
		return nil
	case j.Op != OpRebuild:
		return kubeerr.New(kubeerr.Interrupted, "an earlier %s job of this instance left its journal behind; destroy the instance and create it again", j.Op)
	}
	backend.ReportStep(ctx, fleet.StepRollback, "finishing the undo of the earlier rebuild")
	_, err = rb.p.UndoRebuild(ctx, j, func(s string) { backend.ReportStep(ctx, fleet.StepRollback, s) })
	var stale *StaleJournalError
	switch {
	case errors.As(err, &stale):
		flog.Info("kube rebuild: discarded an earlier rebuild's journal the cluster no longer matches", "cluster", rb.id.cluster, "key", rb.id.key, "why", stale.Why)
	case err != nil:
		return fmt.Errorf("undoing the earlier failed rebuild: %w", err)
	default:
		flog.Info("kube rebuild: finished undoing the earlier failed rebuild", "cluster", rb.id.cluster, "key", rb.id.key, "gen", j.Gen-1)
	}
	if err := RemoveJournal(JournalPath(rb.id.key)); err != nil {
		return err
	}
	// The undo put back the record of the generation it restored.
	prev, err := LoadInstanceRecord(rb.id.cluster, rb.id.key)
	if err != nil {
		return err
	}
	rb.prev = prev
	rb.gen, rb.prevGen = prev.Gen+1, prev.Gen
	rb.holder = rebuildHolder(rb.id.key, rb.gen)
	return nil
}

// begin starts the journal: from here on every change the rebuild makes is
// recorded first, and undone if the rebuild fails. The StatefulSet is the
// anchor that carries the job's mark.
func (rb *rebuilding) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
	replicas := rb.replicas
	status := fleet.StatusStopped
	if rb.running() {
		status = fleet.StatusRunning
	}
	prev := rb.prev
	jj, err := BeginJournal(Journal{
		Op: OpRebuild, Fleet: rb.id.fleet, Instance: rb.id.name, Cluster: rb.id.cluster, Namespace: rb.ns, Key: rb.id.key,
		Gen: rb.gen, Step: stepPrepare,
		Objects:      []JournalObject{{Kind: KindStatefulSet, Name: rb.sts.Name, UID: string(rb.sts.UID), Gen: rb.prevGen, Anchor: true}},
		PrevReplicas: &replicas, PrevSpecSecret: names.SpecSecret(rb.sts.Name, rb.prevGen),
		PrevStatus: status, PrevRecord: &prev,
	})
	if errors.Is(err, ErrJournalExists) {
		return kubeerr.New(kubeerr.Interrupted, "an earlier job of this instance left its journal behind; destroy the instance and create it again")
	}
	if err != nil {
		return err
	}
	rb.jj = jj
	s, _ := rb.scope()
	return classify(s.MirrorStep(ctx, jj.Journal()))
}

// reseed is the journaled part of the rebuild.
func (rb *rebuilding) reseed(ctx context.Context, adm *podspec.Admitted, r podspec.RenderInput, img *built, cfg *devcli.Config, deferred bool) (*backend.UpResult, error) {
	if rb.running() {
		if err := rb.step(ctx, stepStop, fleet.StepBackendDown, "stopping the instance for its new root"); err != nil {
			return nil, err
		}
		if err := rb.p.Scale(ctx, rb.id.cluster, rb.sts.Name, 0); err != nil {
			return nil, classify(err)
		}
		s, _ := rb.scope()
		if err := rb.p.waitPodGone(ctx, s, names.Pod(rb.sts.Name)); err != nil {
			return nil, classify(err)
		}
	}
	if deferred {
		var err error
		if adm, r, err = rb.admit(ctx, img, cfg); err != nil {
			return nil, err
		}
	}

	if err := rb.step(ctx, stepApply, fleet.StepApplyManifests, ""); err != nil {
		return nil, err
	}
	if err := rb.recordRollback(r); err != nil {
		return nil, err
	}
	if err := rb.applyGeneration(ctx, adm); err != nil {
		return nil, err
	}

	if err := rb.step(ctx, StepReset, fleet.StepSeed, fmt.Sprintf("generation %d", rb.gen)); err != nil {
		return nil, err
	}
	manifest, err := rb.seed(ctx, adm, img, rb.seedSpec(adm, img.capture))
	if err != nil {
		return nil, err
	}
	return rb.launch(ctx, adm, r, manifest, img, cfg)
}

// recordRollback records what undoing the rebuild past its reset needs: the
// StatefulSet as it is now (generation prevGen), and the tool Job that puts
// prevGen's root back from trash, rendered to run the image prevGen runs
// where prevGen's pods run (toolResult): the new generation's placement (an
// architecture, a runtime class) may be what it failed on, and must not keep
// the rollback from scheduling on the disk's node too.
func (rb *rebuilding) recordRollback(r podspec.RenderInput) error {
	stsBody, err := statefulSetBody(rb.sts)
	if err != nil {
		return err
	}
	ri := r
	ri.Gen, ri.CreatedAt, ri.Image, ri.Seed = rb.prevGen, rb.prev.Created, liveImage(rb.sts, rb.prev), nil
	job, err := podspec.RenderJob(toolResult(rb.sts), ri, podspec.JobInput{Name: names.RollbackJob(rb.sts.Name, rb.prevGen), Resources: toolResources()}, names.RoleTool)
	if err != nil {
		return classify(err)
	}
	jobBody, err := podspec.ApplyBody(job.Job)
	if err != nil {
		return err
	}
	return rb.jj.Update(func(j *Journal) { j.Rollback = &Rollback{StatefulSet: stsBody, Job: jobBody} })
}

// statefulSetBody is the apply body that puts sts back as it is: its labels,
// annotations (but the job's mark) and spec, nothing the API server keeps
// of its own.
func statefulSetBody(sts *appsv1.StatefulSet) ([]byte, error) {
	annotations := map[string]string{}
	for k, v := range sts.Annotations {
		if k != names.AnnotationOp {
			annotations[k] = v
		}
	}
	return podspec.ApplyBody(&appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Name: sts.Name, Namespace: sts.Namespace, Labels: sts.Labels, Annotations: annotations},
		Spec:       *sts.Spec.DeepCopy(),
	})
}

// liveImage is the image generation prevGen's pods run, as its StatefulSet
// names it (a gateway's re-pointed reference included), with its pull
// policy and Secrets: what a tool Job of that generation runs.
func liveImage(sts *appsv1.StatefulSet, prev InstanceRecord) podspec.ImageRef {
	ref := podspec.ImageRef{Ref: prev.Image}
	if len(prev.ImageDigests) > 0 {
		ref.ID = prev.ImageDigests[0]
	}
	tpl := sts.Spec.Template.Spec
	for _, c := range tpl.Containers {
		if c.Name == DevContainer {
			ref.Ref, ref.PullPolicy = c.Image, c.ImagePullPolicy
		}
	}
	for _, s := range tpl.ImagePullSecrets {
		ref.PullSecrets = append(ref.PullSecrets, s.Name)
	}
	return ref
}

// toolResources are a tool Job's: little, since it runs beside the instance
// (the commit) or wants nothing of what the instance asks for (a GPU, the
// rebuilt devcontainer's larger host requirements).
func toolResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

// rebuildHolder holds the image a rebuild to gen delivered until the rebuild
// succeeds (kubeimage): the instance's key still holds the one it runs.
func rebuildHolder(key string, gen int) string { return key + "@g" + strconv.Itoa(gen) }

// releaseRebuildHold lets go of the image a rebuild to gen delivered: the
// rebuild did not succeed.
func releaseRebuildHold(clusterID, key string, gen int) {
	if err := currentEnvironment().images().Release(clusterID, rebuildHolder(key, gen)); err != nil {
		flog.Warn("kube rebuild: releasing the rebuild's image", "cluster", clusterID, "key", key, "gen", gen, "err", err)
	}
}

// releaseRebuildHolds lets go, for a destroy, of the image a rebuild of
// instance key that never ended holds (its undo failed and kept its journal,
// or the destroy cancelled the reconciler's undo): its holder names the
// generation it made, the one fleetd's record of the pod is at or the next.
func releaseRebuildHolds(clusterID, key string) {
	rec, err := LoadInstanceRecord(clusterID, key)
	if err != nil {
		return
	}
	for _, gen := range []int{rec.Gen, rec.Gen + 1} {
		releaseRebuildHold(clusterID, key, gen)
	}
}

// The outcomes a failed rebuild's error starts with.
const (
	unchangedOutcome = "the rebuild failed before it changed the instance"
	restoredOutcome  = "the rebuild failed; generation %d was restored"
)

// unchanged is a rebuild that failed before its journal began: nothing of
// the instance changed (the daemon host's checkout may hold the export).
func (rb *rebuilding) unchanged(err error) error {
	if rb.delivered {
		releaseRebuildHold(rb.id.cluster, rb.id.key, rb.gen)
	}
	return rebuildFailed(err, unchangedOutcome)
}

// rebuildFailed is a failed rebuild's error: kubernetes:rebuild_failed, what
// became of the instance first (the line a row and the recovery dialog
// show), then the failure (the step's own code in it).
func rebuildFailed(err error, outcome string) error {
	if code, ok := kubeerr.Of(err); ok && code.Is(kubeerr.RebuildFailed) {
		return err
	}
	return &kubeerr.Error{Code: kubeerr.RebuildFailed, Err: fmt.Errorf("%s: %w", outcome, err)}
}

// failed ends a rebuild that failed after its journal began. The daemon's
// shutdown leaves it at its journal for the next start to undo; a destroy
// leaves it to the destroy (which deletes everything). Any other failure is
// undone now: the instance is at its previous generation again, failed
// kubernetes:rebuild_failed; one whose undo fails too keeps its journal, and
// the next start (or a retry) undoes it again.
func (rb *rebuilding) failed(ctx context.Context, err error) error {
	step := rb.jj.Journal().Step
	if backend.ShutdownCut(ctx) {
		flog.Info("kube rebuild: the daemon is shutting down; the job is left for the next start to undo", "fleet", rb.id.fleet, "instance", rb.id.name, "cluster", rb.id.cluster, "key", rb.id.key, "step", step)
		return kubeerr.Wrap(kubeerr.Interrupted, fmt.Errorf("the daemon shut down during the rebuild (at step %s): %w", step, backend.ErrCheckpointed))
	}
	if ctx.Err() != nil {
		rb.endJournal()
		releaseRebuildHold(rb.id.cluster, rb.id.key, rb.gen)
		return err
	}
	backend.ReportStep(ctx, fleet.StepRollback, fmt.Sprintf("the rebuild failed at step %s; restoring generation %d", step, rb.prevGen))
	uctx, cancel := context.WithTimeout(rb.p.ctx, undoTimeout)
	defer cancel()
	_, uerr := rb.p.UndoRebuild(uctx, rb.jj.Journal(), func(s string) { backend.ReportStep(ctx, fleet.StepRollback, s) })
	var stale *StaleJournalError
	switch {
	case errors.As(uerr, &stale):
		// The job's mark never reached the StatefulSet: nothing past the
		// journal's start happened.
		rb.endJournal()
		releaseRebuildHold(rb.id.cluster, rb.id.key, rb.gen)
		return rebuildFailed(err, unchangedOutcome)
	case uerr != nil:
		flog.Error("kube rebuild: undoing the failed rebuild failed; its journal is kept", "fleet", rb.id.fleet, "instance", rb.id.name, "cluster", rb.id.cluster, "key", rb.id.key, "err", uerr)
		return kubeerr.New(kubeerr.RebuildFailed, "undoing the failed rebuild failed: %v (the rebuild failed at step %s: %v); it is tried again when fleetd next starts, or by rebuilding again", uerr, step, err)
	}
	if ferr := rb.jj.Finish(); ferr != nil {
		flog.Warn("kube rebuild: removing the undone rebuild's journal", "cluster", rb.id.cluster, "key", rb.id.key, "err", ferr)
	}
	flog.Info("kube rebuild: failed and undone", "fleet", rb.id.fleet, "instance", rb.id.name, "cluster", rb.id.cluster, "gen", rb.prevGen, "step", step)
	return rebuildFailed(err, fmt.Sprintf(restoredOutcome, rb.prevGen))
}

// endJournal ends the journal of a rebuild nothing will undo: its mark off
// the StatefulSet, its file removed.
func (rb *rebuilding) endJournal() {
	ctx, cancel := rb.cleanupContext()
	defer cancel()
	if s, err := rb.scope(); err == nil {
		if err := s.ClearMark(ctx, rb.jj.Journal()); err != nil && !errors.Is(err, context.Canceled) {
			flog.Warn("kube rebuild: clearing the job's mark", "cluster", rb.id.cluster, "key", rb.id.key, "err", err)
		}
	}
	if err := rb.jj.Finish(); err != nil {
		flog.Warn("kube rebuild: removing the job's journal", "cluster", rb.id.cluster, "key", rb.id.key, "err", err)
	}
}

// commit ends a rebuild that succeeded: its journal first (from here on it
// is never undone), then, in the background, the trash of the previous
// generation (a commit Job beside the pod, pinned to its node), that
// generation's Secrets, and the image hold handed to the instance's key.
// What fails here is logged and left: the next rebuild's commit deletes any
// trash older than its own generation.
func (rb *rebuilding) commit(adm *podspec.Admitted, r podspec.RenderInput, res *backend.UpResult) {
	rb.endJournal()
	if err := currentEnvironment().images().MoveHold(rb.id.cluster, rb.holder, rb.id.key); err != nil {
		flog.Warn("kube rebuild: handing the new image to the instance", "cluster", rb.id.cluster, "key", rb.id.key, "err", err)
	}
	go rb.deleteTrash(adm, r)
}

// deleteTrash runs the commit Job and deletes the previous generation's
// Secrets and seed Job.
func (rb *rebuilding) deleteTrash(adm *podspec.Admitted, r podspec.RenderInput) {
	ctx, cancel := context.WithTimeout(rb.p.ctx, commitTimeout)
	defer cancel()
	s, err := rb.scope()
	if err != nil {
		return
	}
	log := rb.p.opts.Logger
	for _, o := range []objectMeta{
		{Kind: KindSecret, Name: names.SpecSecret(rb.sts.Name, rb.prevGen)},
		{Kind: KindSecret, Name: names.EnvSecret(rb.sts.Name, rb.prevGen)},
		{Kind: KindJob, Name: names.SeedJob(rb.sts.Name, rb.prevGen)},
	} {
		if _, err := s.deleteAll(ctx, []objectMeta{o}); err != nil {
			log.Warn("kube rebuild: deleting the previous generation's object", "cluster", rb.id.cluster, "object", objectRef(o.Kind, o.Name), "err", err)
		}
	}
	if err := rb.runCommit(ctx, s, adm, r); err != nil {
		log.Warn("kube rebuild: deleting the previous generation's root failed; it stays on the disk until the next rebuild", "fleet", rb.id.fleet, "instance", rb.id.name, "cluster", rb.id.cluster, "gen", rb.prevGen, "err", err)
		return
	}
	log.Info("kube rebuild: deleted the previous generation's root", "fleet", rb.id.fleet, "instance", rb.id.name, "cluster", rb.id.cluster, "gen", rb.prevGen)
}

// runCommit runs the commit Job (podseed's OpCommit of generation gen) on the
// node the rebuilt pod runs on, which has the instance disk attached.
func (rb *rebuilding) runCommit(ctx context.Context, s Scope, adm *podspec.Admitted, r podspec.RenderInput) error {
	pod, err := rb.cs.CoreV1().Pods(rb.ns).Get(ctx, names.Pod(rb.sts.Name), metav1.GetOptions{})
	if err != nil {
		return err
	}
	ri := r
	ri.Seed = nil
	if node := pod.Spec.NodeName; node != "" {
		ri.Affinity = onNode(node)
	}
	name := names.CommitJob(rb.sts.Name, rb.gen)
	job, err := podspec.RenderJob(adm.Result, ri, podspec.JobInput{Name: name, Resources: toolResources(), ActiveDeadline: commitTimeout}, names.RoleTool)
	if err != nil {
		return err
	}
	if err := rb.p.freshJob(ctx, s, name); err != nil {
		return err
	}
	body, err := podspec.ApplyBody(job.Job)
	if err != nil {
		return err
	}
	if _, err := applyJob(ctx, rb.cs, rb.ns, body); err != nil {
		return err
	}
	defer rb.p.dropJob(s, name)
	res, err := rb.p.RunJob(ctx, JobRun{
		Cluster: rb.id.cluster, Job: name,
		Spec: podseed.JobSpec{Op: podseed.OpCommit, Gen: rb.gen, PVC: podspec.JobDiskPath},
	})
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	return nil
}

// onNode is a required node affinity to node.
func onNode(node string) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
		}}},
	}}
}

// freshJob makes way for a tool Job named name: one an earlier attempt left
// (finished, waiting out its TTL, or still running) is deleted and waited
// out, since a Job's template cannot be applied over.
func (p *Pool) freshJob(ctx context.Context, s Scope, name string) error {
	existing, err := s.Client.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{})
	if gone(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Job %s: %w", name, err)
	}
	if _, err := s.deleteAll(ctx, []objectMeta{metaOf(KindJob, existing)}); err != nil {
		return err
	}
	return p.waitJobGone(ctx, s, name)
}

// dropJob deletes a tool Job that has done its work (best-effort: its TTL
// deletes it otherwise).
func (p *Pool) dropJob(s Scope, name string) {
	ctx, cancel := context.WithTimeout(p.ctx, finishTimeout)
	defer cancel()
	if _, err := s.deleteAll(ctx, []objectMeta{{Kind: KindJob, Name: name}}); err != nil {
		flog.Warn("kube rebuild: deleting a tool Job", "cluster", s.Cluster, "job", name, "err", err)
	}
}
