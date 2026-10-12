//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// create.go is an instance's create on its cluster (UpContext), after the
// create job has cloned the workspace on the daemon host:
//
//  1. the devcontainer configuration is read (a compose devcontainer is
//     refused here, before any build), and the job's journal begun: from here
//     on every API server write is recorded before it is made, so a daemon
//     that dies mid-create leaves a record the next one fails it by;
//  2. a pre-build dry-run has the cluster's admission judge a stand-in of the
//     pod (pod security, webhooks, quotas), so a create it would refuse fails
//     before the build;
//  3. the image is built on the daemon host by the devcontainer CLI through
//     the capture-mode docker shim, which records the final docker run, then
//     delivered to the cluster (passthrough, node load, registry or gateway);
//  4. the run is translated into the instance's objects and admitted (the
//     debug grants dropped, with a warning, where admission refuses only
//     them);
//  5. the disk, the spec and env Secrets and the seed Job are applied; the
//     seed copies the image root onto the disk, the workspace in;
//  6. the StatefulSet, rendered over what the seed made, starts the pod, which
//     is waited for (deadlines name what it was stuck on), verified against
//     fleetd's record, and given its agent;
//  7. the devcontainer's lifecycle hooks run through `devcontainer set-up`.
//
// The create job's own provisioning (fleet.rc, dotfiles, the agent hooks)
// follows, as on docker. A create that fails leaves its objects, labelled by
// the instance's key, for a destroy to delete (DownByKey); its pod, if it
// got one, is scaled to 0 so a failed instance costs no compute.

// The steps a create journals (Journal.Step).
const (
	stepPreflight Step = "preflight"
	stepBuild     Step = "build"
	stepDeliver   Step = "deliver"
	stepAdmit     Step = "admit"
	stepApply     Step = "apply"
	stepSeed      Step = "seed"
	stepStart     Step = "start"
	stepAgent     Step = "agent"
	stepHooks     Step = "hooks"
)

// finishTimeout bounds clearing a finished job's mark and scaling a failed
// one's pod down, which run after the job's own context may have ended.
const finishTimeout = 30 * time.Second

// cleanupContext bounds a create's clean-up (finishTimeout), which outlives
// its job's context but not the pool: the daemon's bounded shutdown ends it
// with the pool, past the checkpoint (Pool.Shutdown).
func (c *creation) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.p.ctx, finishTimeout)
}

// UpContext creates the instance on its cluster (backend.UpContexter): see
// create.go. ctx is the create job's: cancelling it (a destroy, the daemon
// shutting down) stops the create at its next wait. Its provisioning log and
// steps go to ctx's job log.
func (b *Backend) UpContext(ctx context.Context, req backend.UpRequest) (*backend.UpResult, error) {
	id, err := b.identity()
	if err != nil {
		return nil, err
	}
	c := newCreation(b, id, req)
	res, err := c.run(ctx)
	if err != nil {
		flog.Error("kube create failed", "fleet", id.fleet, "instance", id.name, "cluster", id.cluster, "key", id.key, "ms", time.Since(c.start).Milliseconds(), "err", err)
		return nil, err
	}
	flog.Info("kube create: instance running", "fleet", id.fleet, "instance", id.name, "cluster", id.cluster, "container", res.ContainerID, "ms", time.Since(c.start).Milliseconds(), "seed_cache", c.seedCacheUse)
	return res, nil
}

// newCreation is a create of generation 1 of the instance id names.
func newCreation(b *Backend, id bound, req backend.UpRequest) *creation {
	return &creation{b: b, p: b.p(), id: id, env: currentEnvironment(), req: req, start: time.Now(),
		gen: 1, created: id.created, holder: id.key}
}

// creation is one create in progress, or the generation a rebuild makes.
type creation struct {
	b     *Backend
	p     *Pool
	id    bound
	env   Environment
	req   backend.UpRequest
	start time.Time

	cluster  *Cluster
	cs       kclient.Interface
	ns       string
	rec      fleet.KubeCluster
	daemonID string
	owner    string
	facts    Facts
	identity podspec.Identity

	// gen is the root generation the job makes (1 for a create, N+1 for a
	// rebuild), created the time its container counts as created (the
	// container-created annotation set-up's creation markers compare): the
	// instance's own for a create, the rebuild's for a rebuild, whose
	// creation hooks run again.
	gen     int
	created time.Time
	// prevGen is the generation a rebuild replaces (0 for a create): the
	// seed resets it to trash and keeps the workspace.
	prevGen int
	// holder holds the job's image (kubeimage): the instance's key for a
	// create, a holder of the rebuild's own until it succeeds.
	holder string

	jj *JobJournal
	// started: the StatefulSet was applied, so a failure scales it down.
	started bool
	// checkpointed: the daemon's shutdown cut the create, which leaves its
	// journal for the next start (checkpoint).
	checkpointed bool
	warnings     []string
	// nodeImage is the id the node gave a node-loaded image, learned from
	// the seed Job's pod (the first to run it).
	nodeImage string
	// seedCacheUse says how the create used its image's seed cache, for
	// its outcome line ("" without one).
	seedCacheUse string
	// verb is what the job is, for its messages ("" is "create"; a clone's
	// is "clone").
	verb string
	// volumes releases the claim on the fleet's named volumes the seed
	// fills (share_volumes.go) once the record names them; nil for none.
	volumes func()
	// share is where the instance's fleet mounts live (share.go); nil
	// without any.
	share *shareState
	// keptDiskClass is the StorageClass of the disk a rebuild keeps (""
	// for a new disk: diskClass resolves it).
	keptDiskClass string
	// moveHolds hold off a move of the fleet's share until the job ends
	// (shareMoveGate; releaseMoveHolds).
	moveHolds []func()
}

// what is the job, for its messages.
func (c *creation) what() string {
	if c.verb != "" {
		return c.verb
	}
	return "create"
}

func (c *creation) run(ctx context.Context) (*backend.UpResult, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	defer c.releaseMoveHolds()
	if err := c.prepareShare(ctx); err != nil {
		return nil, err
	}
	// The share may be prepared only once the run is translated (admit).
	defer func() { c.share.done() }()
	defer c.releaseVolumes()
	cfg, err := devcli.ReadMerged(ctx, devcli.ReadRequest{Tools: c.env.Tools, WorkspaceDir: c.req.WorkspaceDir})
	if err != nil {
		return nil, kubeerr.Wrap(kubeerr.ImageBuild, err)
	}

	jj, err := BeginJournal(Journal{Op: OpCreate, Fleet: c.id.fleet, Instance: c.id.name, Cluster: c.id.cluster, Namespace: c.ns, Key: c.id.key, Gen: c.gen, Step: stepPreflight})
	if errors.Is(err, ErrJournalExists) {
		return nil, kubeerr.New(kubeerr.Interrupted, "an earlier job of this instance left its journal behind; destroy the instance and create it again")
	}
	if err != nil {
		return nil, err
	}
	c.jj = jj
	defer c.finish()

	res, err := c.provision(ctx, cfg)
	if err != nil {
		return nil, c.failed(ctx, err)
	}
	res.Warnings = c.warningLines()
	return res, nil
}

// failed ends a create that failed with err after its journal began: its
// started pod is scaled down, and its journal finished (finish). One the
// daemon's shutdown cut is left at a checkpoint instead.
func (c *creation) failed(ctx context.Context, err error) error {
	if backend.ShutdownCut(ctx) {
		return c.checkpoint()
	}
	c.stopFailed()
	return err
}

// checkpoint leaves a create the daemon's shutdown cut where it stopped: its
// journal stays, naming its step and every object it wrote, and its record
// stays creating, so the next start's reconciler fails it
// kubernetes:interrupted at that step, exactly as after a daemon kill; the
// shutdown spends none of its budget unwinding it. Its started pod is still
// scaled down while the pool lasts, so the failed instance costs no compute.
func (c *creation) checkpoint() error {
	c.checkpointed = true
	c.stopFailed()
	step := c.jj.Journal().Step
	flog.Info("kube "+c.what()+": the daemon is shutting down; the job is left for the next start", "fleet", c.id.fleet, "instance", c.id.name, "cluster", c.id.cluster, "key", c.id.key, "step", step)
	return kubeerr.Wrap(kubeerr.Interrupted, fmt.Errorf("the daemon shut down during the %s (at step %s): %w", c.what(), step, backend.ErrCheckpointed))
}

// provision is the create after its journal began.
func (c *creation) provision(ctx context.Context, cfg *devcli.Config) (*backend.UpResult, error) {
	if err := c.preflight(ctx, cfg); err != nil {
		return nil, err
	}
	img, err := c.build(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := c.step(ctx, stepAdmit, fleet.StepApplyManifests, "admission"); err != nil {
		return nil, err
	}
	adm, r, err := c.admit(ctx, img, cfg)
	if err != nil {
		return nil, err
	}

	if err := c.step(ctx, stepApply, fleet.StepApplyManifests, ""); err != nil {
		return nil, err
	}
	o := adm.Objects
	spec := c.seedSpec(adm, img.capture)
	cache := c.seedCacheFor(ctx, spec, img.capture.Image.ID, o.Disk)
	if cache != nil && cache.build {
		defer seedCaches.release(c.id.cluster, cache.id)
	}
	if cache != nil && cache.clone {
		cache.cloneFrom(o.Disk)
	}
	applied := time.Now()
	if err := c.apply(ctx, KindPVC, o.Disk.Name, true, o.Disk); err != nil {
		return nil, err
	}
	if err := c.applyGeneration(ctx, adm); err != nil {
		return nil, err
	}

	if err := c.step(ctx, stepSeed, fleet.StepSeed, ""); err != nil {
		return nil, err
	}
	if cache != nil && cache.clone {
		backend.ReportStep(ctx, fleet.StepSeed, "cloning the disk from seed cache "+cache.name)
		if err := c.seedFromCache(ctx, o, spec, cache, applied); err != nil {
			return nil, err
		}
	}
	manifest, err := c.seed(ctx, adm, img, spec)
	if err != nil {
		return nil, err
	}
	if cache != nil && cache.build {
		c.buildSeedCache(ctx, adm, r, img.del, cache)
	}
	return c.launch(ctx, adm, r, manifest, img, cfg)
}

// built is the job's image: capture-up's result, delivered.
type built struct {
	capture *devcli.CaptureResult
	del     *kubeimage.Delivery
}

// build builds the image on the daemon host (capture-up) and delivers it to
// the cluster, held by c.holder.
func (c *creation) build(ctx context.Context, cfg *devcli.Config) (*built, error) {
	if err := c.step(ctx, stepBuild, fleet.StepImageBuild, ""); err != nil {
		return nil, err
	}
	capture, err := devcli.CaptureUp(ctx, devcli.CaptureRequest{
		Tools: c.env.Tools, WorkspaceDir: c.req.WorkspaceDir, Config: cfg,
		Platform: c.facts.Platform, GPU: false,
	})
	if err != nil {
		return nil, kubeerr.Wrap(kubeerr.ImageBuild, err)
	}
	if c.identity.NonRoot && kubeimage.BuiltBy(capture.Calls) {
		c.addAudioLayer(ctx, capture)
	}

	if err := c.step(ctx, stepDeliver, fleet.StepImageDeliver, capture.Run.Image); err != nil {
		return nil, err
	}
	t, err := c.imageTarget()
	if err != nil {
		return nil, err
	}
	del, err := c.env.images().Deliver(ctx, t, kubeimage.NewImage(capture.Run.Image, capture.Image, capture.Calls), c.holder)
	if err != nil {
		return nil, kubeerr.Wrap(kubeerr.ImagePush, err)
	}
	for _, w := range del.Warnings {
		c.warn(ctx, w)
	}
	return &built{capture: capture, del: del}, nil
}

// pendingFacts is what explains the create's pods kept unscheduled.
func (c *creation) pendingFacts(ctx context.Context) PendingFacts {
	f := PendingFacts{Capabilities: c.facts.Capabilities, Cluster: c.rec, SingleNode: c.share.modeOf() == topology.SingleNode}
	if f.SingleNode && c.p != nil {
		// The fleet's instances that want to run, this one among them
		// (its StatefulSet is not made before its seed ran).
		n, found := c.p.groupWanting(ctx, c.id.cluster, names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet), c.id.sts)
		if !found {
			n++
		}
		f.Instances = n
	}
	return f
}

// reportPending says in the provisioning log what the scheduler keeps the
// create's pod (step's) waiting on, the node's room named when that is it.
func (c *creation) reportPending(step fleet.JobStep) func(context.Context, string) {
	return func(ctx context.Context, reason string) {
		if note, _, ok := ExplainUnschedulable(reason, c.pendingFacts(ctx)); ok {
			reason = note
		}
		backend.ReportStep(ctx, step, "pending: "+reason)
	}
}

// admit translates the captured run into generation c.gen's objects, as the
// cluster's admission takes them (debug grants dropped where only they are
// refused), and returns them with their render's input.
func (c *creation) admit(ctx context.Context, img *built, cfg *devcli.Config) (*podspec.Admitted, podspec.RenderInput, error) {
	in := c.input(img.capture.Capture, img.capture.Image, cfg)
	in.LookupGroup = c.groupsOf(ctx, img.capture.Image.ID).lookup(ctx)
	res, err := c.translate(ctx, in)
	if err != nil {
		return nil, podspec.RenderInput{}, err
	}
	r := c.renderInput(imageRefOf(img.del), c.seedJob(ctx, img.capture.Image.ID, podspec.SharedVolumes(res)))
	adm, err := podspec.Admit(ctx, c.cs, in, r)
	if err != nil {
		return nil, r, classify(err)
	}
	for _, w := range append(append([]string(nil), adm.Result.Pod.Warnings...), adm.Objects.Warnings...) {
		c.warn(ctx, w)
	}
	return adm, r, nil
}

// applyGeneration applies generation c.gen's spec and env Secrets and its
// seed Job.
func (c *creation) applyGeneration(ctx context.Context, adm *podspec.Admitted) error {
	o := adm.Objects
	for _, s := range []*corev1.Secret{o.SpecSecret, o.EnvSecret} {
		if err := c.apply(ctx, KindSecret, s.Name, false, s); err != nil {
			return err
		}
	}
	return c.apply(ctx, KindJob, o.Seed.Job.Name, false, o.Seed.Job)
}

// launch starts generation c.gen once the seed made its root: the
// StatefulSet rendered over the seeded layout, fleetd's record of its pod,
// the pod waited for and verified, its agent, the shared state, the
// devcontainer's lifecycle hooks.
func (c *creation) launch(ctx context.Context, adm *podspec.Admitted, r podspec.RenderInput, manifest *podseed.Manifest, img *built, cfg *devcli.Config) (*backend.UpResult, error) {
	capture, del := img.capture, img.del
	if err := c.step(ctx, stepStart, fleet.StepWaitPod, ""); err != nil {
		return nil, err
	}
	r.Root, r.Seed = podspec.RootLayout{Dirs: manifest.Dirs, Extra: manifest.RootExtra}, nil
	// Placed now: a share the seed bound has its topology only since.
	r.Placement = c.share.placementFor()
	o, err := podspec.Render(adm.Result, r)
	if err != nil {
		return nil, classify(err)
	}
	rec := InstanceRecord{
		Cluster: c.id.cluster, Namespace: c.ns, Key: c.id.key, Fleet: c.id.fleet, Instance: c.id.name,
		StatefulSet: c.id.sts, Gen: c.gen, SpecHash: podspec.SpecHash(&o.StatefulSet.Spec.Template.Spec),
		Image: del.Ref, Strategy: string(del.Strategy), RemoteUser: capture.RemoteUser,
		WorkspaceFolder: cfg.WorkspaceFolder, NonRoot: c.identity.NonRoot, Created: c.created,
		Ports:       publishedPorts(adm.Result.Pod.Ports),
		HostSockets: recordedHostSockets(adm.Result.Pod.HostSockets),
		HostGateway: adm.Result.Pod.HostGateway, GatewaySkip: adm.Result.Pod.GatewaySkip,
		Share:       c.share.record(&adm.Result.Pod),
		DockerImage: dockerImageOf(o),
	}
	rec.addImageDigest(del.Digest)
	rec.addImageDigest(del.ImageID)
	rec.addImageDigest(c.nodeImage)
	c.started = true
	// A create makes the StatefulSet; a rebuild changes the one it has,
	// and mirrors its steps on it.
	rebuild := c.prevGen > 0
	uid, err := c.applyJournaled(ctx, JournalObject{Kind: KindStatefulSet, Name: o.StatefulSet.Name, Gen: c.gen, Created: !rebuild, Anchor: rebuild}, o.StatefulSet)
	if err != nil {
		return nil, err
	}
	rec.StatefulSetUID = uid
	if b := o.DisruptionBudget; b != nil {
		created := true
		if rebuild {
			_, err := c.cs.PolicyV1().PodDisruptionBudgets(c.ns).Get(ctx, b.Name, metav1.GetOptions{})
			created = gone(err)
		}
		if _, err := c.applyJournaled(ctx, JournalObject{Kind: KindPDB, Name: b.Name, Gen: c.gen, Created: created}, b); err != nil {
			return nil, err
		}
	}
	if err := SaveInstanceRecord(rec); err != nil {
		return nil, err
	}
	// The record names the fleet's named volumes the seed filled now.
	c.releaseVolumes()

	pulls := newGatewayPulls(c.p, del.Strategy, c.id.cluster, c.id.sts)
	failedCreate := c.failedCreate(o.StatefulSet.Name)
	pod, err := c.p.WaitReady(ctx, c.id.cluster, c.id.sts, WaitOptions{PVC: o.Disk.Name, Check: func(ctx context.Context) error {
		if err := failedCreate(ctx); err != nil {
			return err
		}
		return pulls.check(ctx)
	}, Pending: c.reportPending(fleet.StepWaitPod)})
	if err != nil {
		return nil, classify(explainWith(pulls.explain(err), c.pendingFacts(ctx)))
	}
	if err := c.learnNodeImage(pod, &rec, del); err != nil {
		return nil, err
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

	if err := c.step(ctx, stepHooks, fleet.StepLifecycleHooks, ""); err != nil {
		return nil, err
	}
	containerID := ContainerID(c.id.cluster, c.ns, c.id.sts, c.gen)
	err = setUp{
		session: sess, pod: pod, container: capture.Container, containerID: containerID,
		created: c.created, workspaceFolder: cfg.WorkspaceFolder, tools: c.env.Tools,
	}.run(ctx)
	if err != nil {
		return nil, fmt.Errorf("the devcontainer's lifecycle hooks: %w", err)
	}
	// After the hooks, which may install the image's own tmux.
	if err := ensureTmuxOn(ctx, sess); err != nil {
		c.warn(ctx, fmt.Sprintf("tmux for the instance's sessions: %v", err))
	}
	if c.identity.NonRoot {
		c.checkAudio(ctx, sess, &rec)
	}
	return &backend.UpResult{Outcome: "success", ContainerID: containerID, RemoteUser: capture.RemoteUser, RemoteWorkspaceFolder: cfg.WorkspaceFolder}, nil
}

// connect loads the instance's connection and what its preflight found.
func (c *creation) connect(ctx context.Context) error {
	cl, err := c.p.Cluster(c.id.cluster)
	if err != nil {
		return classify(err)
	}
	cs, err := c.p.clientset(cl)
	if err != nil {
		return classify(err)
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return fmt.Errorf("the daemon's identity: %w", err)
	}
	facts, err := c.env.facts(ctx, c.id.cluster)
	if err != nil {
		return err
	}
	c.cluster, c.cs, c.ns, c.rec = cl, cs, cl.Namespace(), currentRecord(cl)
	c.daemonID, c.owner, c.facts = ident.DaemonID, ident.Owner(), facts
	c.identity = instanceIdentity(facts.Capabilities)
	return nil
}

// step journals and reports that the create moved to step, after checking
// it was not cancelled meanwhile. Before its journal begins (a rebuild's
// export and build) a step is only reported.
func (c *creation) step(ctx context.Context, step Step, js fleet.JobStep, detail string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
	backend.ReportStep(ctx, js, detail)
	if c.jj == nil {
		return nil
	}
	if err := c.jj.Step(step); err != nil {
		return err
	}
	s, err := c.scope()
	if err != nil {
		return err
	}
	return classify(s.MirrorStep(ctx, c.jj.Journal()))
}

func (c *creation) scope() (Scope, error) {
	return Scope{Client: c.cs, Namespace: c.ns, Owner: c.owner, Cluster: c.id.cluster, Gate: c.p.gateFor(c.id.cluster), Logger: c.p.opts.Logger}, nil
}

// preflight has admission judge a stand-in of the pod before the build.
func (c *creation) preflight(ctx context.Context, cfg *devcli.Config) error {
	backend.ReportStep(ctx, fleet.StepApplyManifests, "pre-build admission check")
	capture, err := podspec.PreflightCapture(cfg.Merged, c.req.WorkspaceDir, cfg.WorkspaceFolder)
	if err != nil {
		return err
	}
	in := c.input(capture, kubeshim.ImageConfig{}, cfg)
	res, err := c.translate(ctx, in)
	if err != nil {
		return err
	}
	r := c.renderInput(podspec.ImageRef{Ref: podspec.PreflightImage, ID: podspec.PreflightImage}, c.seedJob(ctx, "", podspec.SharedVolumes(res)))
	if _, err := podspec.Admit(ctx, c.cs, in, r); err != nil {
		return classify(err)
	}
	return nil
}

// translate translates the run in, and prepares the fleet's share when the
// run keeps something on it besides the fleet's mounts (named volumes,
// bound host folders and files), which only the translation tells.
func (c *creation) translate(ctx context.Context, in podspec.Input) (*podspec.Result, error) {
	res, err := podspec.Translate(in)
	if err != nil {
		return nil, classify(err)
	}
	if err := c.needShare(ctx, &res.Pod); err != nil {
		return nil, err
	}
	return res, nil
}

// input is the translation's input for a captured run. Exec-class commands
// get the in-pod ssh-agent socket as SSH_AUTH_SOCK while the agent has it
// bound (whether the daemon can serve the pod an agent is decided as they
// start, not here).
func (c *creation) input(capture *kubeshim.Capture, image kubeshim.ImageConfig, cfg *devcli.Config) podspec.Input {
	rec := c.rec
	return podspec.Input{
		Capture: capture, Image: image, Merged: cfg.Merged,
		WorkspaceDir: c.req.WorkspaceDir, WorkspaceFolder: cfg.WorkspaceFolder,
		FleetMounts: c.req.Mounts, Cluster: &rec, Facts: clusterFacts(c.facts, c.diskClass()),
		HostAgentSockets: agentsock.HostAgentPaths(),
		SSHAuthSock:      agentsock.ContainerSocketPath,
		MultiNodeShare:   c.share != nil && c.share.mode == topology.MultiNode,
		FleetDir:         state.FleetDir(),
	}
}

// recordedHostSockets is the host-socket binds as the instance record keeps
// them: each source resolved now, so a link that appears on the way later is
// refused rather than followed (hostrelay.Dial). A source that cannot be
// resolved (it went since the translation stat-ed it) is kept as written; a
// dial then finds what is there.
func recordedHostSockets(socks []hostrelay.Socket) []hostrelay.Socket {
	if len(socks) == 0 {
		return nil
	}
	out := make([]hostrelay.Socket, len(socks))
	for i, s := range socks {
		if real, err := hostrelay.Resolve(s.Source); err == nil {
			s.Source = real
		}
		out[i] = s
	}
	return out
}

// renderInput is the render's input for generation c.gen.
func (c *creation) renderInput(image podspec.ImageRef, seed *podspec.JobInput) podspec.RenderInput {
	rec := c.rec
	return podspec.RenderInput{
		Cluster: &rec, Namespace: c.ns, Name: c.id.sts, Key: c.id.key, Owner: c.owner, ClusterID: c.id.cluster,
		FleetName: c.id.fleet, FleetHash: names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet),
		InstanceName: c.id.name, Gen: c.gen, CreatedAt: c.created, Replicas: 1,
		Image: image, StorageClass: c.diskClass(), Identity: c.identity,
		SELinuxLevel: podspec.SELinuxLevel(names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet)),
		SELinuxMount: c.share != nil && c.share.selinuxMount,
		Share:        c.share.input(),
		Placement:    c.share.placementFor(),
		Seed:         seed,
	}
}

// seedJob is the seed Job's input: bounded by the seed deadline for an
// image of its size, the share mounted beside the disk when the seed binds
// it or fills the fleet's named volumes there (sharedVolumes).
func (c *creation) seedJob(ctx context.Context, imageID string, sharedVolumes bool) *podspec.JobInput {
	return &podspec.JobInput{Name: names.SeedJob(c.id.sts, c.gen), ActiveDeadline: DefaultDeadlines().SeedDeadline(c.imageSize(ctx, imageID)), Claims: c.share.seedClaims(sharedVolumes)}
}

// imageSize is the image's size on the daemon host (0 when unknown).
func (c *creation) imageSize(ctx context.Context, imageID string) int64 {
	if imageID == "" {
		return 0
	}
	out, err := exec.CommandContext(ctx, c.docker(), "image", "inspect", "--format", "{{.Size}}", imageID).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// docker is the daemon host's docker.
func (c *creation) docker() string {
	if c.env.Tools.Docker != "" {
		return c.env.Tools.Docker
	}
	return "docker"
}

// groupsOf resolves the run's --group-add names against image imageID's
// /etc/group; a failed read goes to the job's log, beside the refusal of the
// name it leaves unresolved.
func (c *creation) groupsOf(ctx context.Context, imageID string) *imageGroups {
	return &imageGroups{docker: c.docker(), image: imageID, platform: c.facts.Platform, report: func(err error) {
		backend.ReportWarning(ctx, err.Error())
		flog.Warn("kube create: reading the image's groups", "cluster", c.id.cluster, "key", c.id.key, "err", err)
	}}
}

// currentRecord is connection cl's record as kube_clusters.json holds it
// now: the pool keeps a connection's client across a change of the settings
// it does not read (image delivery, resources, storage, ...), so its own copy
// of the record can be older than what a create or start must apply. The
// pool's copy stands in when the registry cannot be read.
func currentRecord(cl *Cluster) fleet.KubeCluster {
	if k, err := state.LoadKubeClusters(); err == nil {
		if rec := k.Find(cl.Record.ID); rec != nil {
			return *rec
		}
	}
	return cl.Record
}

// imageTarget is where the image goes: the connection, as its preflight
// found it, with its managed pull Secrets in its namespace.
func (c *creation) imageTarget() (kubeimage.Target, error) {
	rec := c.rec
	return kubeimage.Target{
		Cluster: &rec, DaemonID: c.daemonID, Local: c.facts.Local, Platform: c.facts.Platform,
		Secrets: PullSecrets{Client: c.cs, Namespace: c.ns}, Labels: PullSecretLabels(c.owner, c.id.cluster),
	}, nil
}

// imageTarget is the image delivery's target for connection c (a Start's
// re-delivery).
func (b *Backend) imageTarget(ctx context.Context, cl *Cluster) (kubeimage.Target, error) {
	env := currentEnvironment()
	facts, err := env.facts(ctx, cl.Record.ID)
	if err != nil {
		return kubeimage.Target{}, err
	}
	cs, err := b.p().clientset(cl)
	if err != nil {
		return kubeimage.Target{}, err
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return kubeimage.Target{}, err
	}
	rec := currentRecord(cl)
	return kubeimage.Target{
		Cluster: &rec, DaemonID: ident.DaemonID, Local: facts.Local, Platform: facts.Platform,
		Secrets: PullSecrets{Client: cs, Namespace: cl.Namespace()}, Labels: PullSecretLabels(ident.Owner(), rec.ID),
	}, nil
}

// imageRefOf is the image the pods pull for a delivery.
func imageRefOf(d *kubeimage.Delivery) podspec.ImageRef {
	ref := podspec.ImageRef{Ref: d.Ref, PullSecrets: d.PullSecrets, ID: d.Digest}
	if ref.ID == "" {
		ref.ID = d.ImageID
	}
	if d.PullPolicy == kubeimage.PullNever {
		ref.PullPolicy = corev1.PullNever
	}
	return ref
}

// apply server-side applies one of the create's objects, journaled: the
// intent before the write, the UID after it. The anchor carries the job's
// mark (fleet.dev/op) from its first write.
func (c *creation) apply(ctx context.Context, kind, name string, anchor bool, obj metav1.Object) error {
	_, err := c.applyUID(ctx, kind, name, anchor, obj)
	return err
}

func (c *creation) applyUID(ctx context.Context, kind, name string, anchor bool, obj metav1.Object) (string, error) {
	return c.applyJournaled(ctx, JournalObject{Kind: kind, Name: name, Gen: c.gen, Created: true, Anchor: anchor}, obj)
}

// applyJournaled server-side applies obj as the journal object jo says (an
// object the job makes, or one it changes), and returns its UID.
func (c *creation) applyJournaled(ctx context.Context, jo JournalObject, obj metav1.Object) (string, error) {
	kind, name, anchor := jo.Kind, jo.Name, jo.Anchor
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
	if err := c.jj.Intend(jo); err != nil {
		return "", err
	}
	if anchor {
		a := obj.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[names.AnnotationOp] = c.jj.Mark()
		obj.SetAnnotations(a)
	}
	body, err := podspec.ApplyBody(obj)
	if err != nil {
		return "", err
	}
	uid, err := applyObject(ctx, c.cs, c.ns, kind, name, body)
	if err != nil {
		return "", classify(fmt.Errorf("applying %s: %w", objectRef(kind, name), err))
	}
	if err := c.jj.Applied(kind, name, uid); err != nil {
		return "", err
	}
	if anchor {
		// The step's mirror goes on the anchor once it exists.
		s, _ := c.scope()
		if err := s.MirrorStep(ctx, c.jj.Journal()); err != nil {
			return "", classify(err)
		}
	}
	return uid, nil
}

// applyObject server-side applies body (fleet's field manager, forced) and
// returns the object's UID.
func applyObject(ctx context.Context, cs kclient.Interface, ns, kind, name string, body []byte) (string, error) {
	force := true
	opts := metav1.PatchOptions{FieldManager: DefaultFieldManager, Force: &force}
	var (
		m   metav1.Object
		err error
	)
	switch kind {
	case KindPVC:
		m, err = cs.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case KindSecret:
		m, err = cs.CoreV1().Secrets(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case KindJob:
		m, err = cs.BatchV1().Jobs(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case KindStatefulSet:
		m, err = cs.AppsV1().StatefulSets(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case KindPDB:
		m, err = cs.PolicyV1().PodDisruptionBudgets(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case KindVolumeSnapshot:
		return applySnapshot(ctx, cs, ns, name, body)
	default:
		return "", fmt.Errorf("applying a %s is not something a create does", kind)
	}
	if err != nil {
		return "", err
	}
	return string(m.GetUID()), nil
}

// seedSpec is the seed Job's work: the image root onto the disk, the
// workspace in, the share uid remap. A rebuild's seed (c.prevGen set) first
// moves the generation it replaces to trash and keeps the workspace on the
// disk as it is.
func (c *creation) seedSpec(adm *podspec.Admitted, capture *devcli.CaptureResult) *podseed.SeedSpec {
	spec := podspec.SeedSpec(adm.Result, adm.Objects)
	uid, gid := shareIDs()
	if c.identity.NonRoot {
		// One uid runs everything: the seed cannot chown, and the pod's
		// uid owns what it wrote.
		spec.Workspace = &podseed.Workspace{}
	} else {
		spec.Workspace = &podseed.Workspace{Owner: &podseed.IDs{UID: uid, GID: gid}}
		if u := capture.RemoteUser; u != "" && u != "root" && u != "0" {
			spec.Remap = &podseed.Remap{User: u, UID: uid, GID: gid}
		}
	}
	if c.prevGen > 0 {
		spec.Reset, spec.PrevGen, spec.Workspace = true, c.prevGen, nil
	}
	if len(spec.SharedVolumes) > 0 {
		// The fleet's named volumes are filled on the share (the copy
		// tier's on the disk).
		if spec.Share = c.share.seedPath(); spec.Share == "" {
			spec.SharedVolumes = nil
		}
	}
	return spec
}

// seed runs the seed Job of spec; it returns the seeded generation's
// manifest. A rebuild's keeps the workspace on the disk: no tar goes in.
func (c *creation) seed(ctx context.Context, adm *podspec.Admitted, img *built, spec *podseed.SeedSpec) (*podseed.Manifest, error) {
	del := img.del
	workspace := workspaceTar(c.req.WorkspaceDir)
	if c.prevGen > 0 {
		workspace = nil
	}
	c.claimSharedVolumes(ctx, spec)
	job := adm.Objects.Seed
	want := job.Job.Spec.Template.Spec
	digests := []string{del.Digest, del.ImageID}
	res, err := c.p.RunJob(ctx, JobRun{
		Cluster:   c.id.cluster,
		Job:       job.Job.Name,
		Spec:      podseed.JobSpec{Op: podseed.OpSeed, Gen: c.gen, PVC: podspec.JobDiskPath, Seed: spec},
		Workspace: workspace,
		PVC:       adm.Objects.Disk.Name,
		Progress: func(pr jobboot.Progress) {
			backend.ReportStep(ctx, fleet.StepSeed, seedProgress(pr))
		},
		Scheduling: c.reportPending(fleet.StepSeed),
		Check: func(pod *corev1.Pod) error {
			id, err := checkJobPod(pod, &want, del, digests)
			if id != "" {
				c.nodeImage = id
			}
			if c.share != nil && c.share.bind {
				c.share.seedNode = pod.Spec.NodeName
			}
			return err
		},
	})
	if serr := c.finishShare(ctx, err == nil && res.OK); serr != nil && err == nil && res.OK {
		return nil, serr
	}
	if err != nil {
		return nil, classify(explainWith(err, c.pendingFacts(ctx)))
	}
	if !res.OK {
		return nil, fmt.Errorf("the seed failed: %s", res.Error)
	}
	if len(res.Output) == 0 {
		return nil, errors.New("the seed finished, but its manifest did not reach fleet")
	}
	m, err := podseed.ParseManifest(res.Output)
	if err != nil {
		return nil, fmt.Errorf("the seed's manifest: %w", err)
	}
	return &m, nil
}

// claimSharedVolumes claims the fleet's named volumes spec's seed fills on
// the share until the record names them, so a destroy of their last other
// instance meanwhile leaves them; one such a destroy left unused is filled
// fresh. The copy tier's are the instance's own.
func (c *creation) claimSharedVolumes(ctx context.Context, spec *podseed.SeedSpec) {
	s := c.share
	if s == nil || s.mode == topology.Copy || len(spec.SharedVolumes) == 0 || c.volumes != nil {
		return
	}
	ids := make([]string, len(spec.SharedVolumes))
	for i, v := range spec.SharedVolumes {
		ids[i] = v.Name
	}
	fresh, release := claimVolumes(ctx, c.cs, c.ns, c.id.cluster, s.claim, ids)
	for i := range spec.SharedVolumes {
		spec.SharedVolumes[i].Fresh = fresh[spec.SharedVolumes[i].Name]
	}
	c.volumes = release
}

// releaseVolumes lets go of the claim claimSharedVolumes took.
func (c *creation) releaseVolumes() {
	if c.volumes != nil {
		c.volumes()
	}
}

// seedProgress words a seed's progress for the job's step line.
func seedProgress(p jobboot.Progress) string {
	switch {
	case p.Files > 0 && p.Detail != "":
		return fmt.Sprintf("%s %s (%d files)", p.Phase, p.Detail, p.Files)
	case p.Files > 0:
		return fmt.Sprintf("%s (%d files)", p.Phase, p.Files)
	case p.Detail != "":
		return p.Phase + " " + p.Detail
	}
	return p.Phase
}

// checkJobPod verifies a Job's pod before it gets anything (the workspace,
// which may hold secrets): its spec is the one fleetd rendered (want) and its
// container runs the image fleetd delivered, one of digests. A node-loaded
// image has no digest before a node gives it an id: its pod runs whatever
// image the node holds under the tag only fleet daemons load (the spec, which
// names that tag with pull policy Never, is fleet's), and checkJobPod returns
// that id to be learned. The status's image name is not compared: a kubelet
// names a running image by any of its tags, and an image whose config another
// load gave the node too (a rebuild, another daemon's load) has several.
func checkJobPod(pod *corev1.Pod, want *corev1.PodSpec, del *kubeimage.Delivery, digests []string) (string, error) {
	var reasons []string
	if podspec.SpecHash(&pod.Spec) != podspec.SpecHash(want) {
		reasons = append(reasons, "its spec differs from fleet's render")
	}
	learned := ""
	if st := containerStatus(pod, podspec.JobContainer); st != nil && st.ImageID != "" && !digestIn(st.ImageID, digests) {
		if del.Strategy == kubeimage.StrategyNodeLoad {
			learned = imageDigest(st.ImageID)
		} else {
			reasons = append(reasons, fmt.Sprintf("its %s container runs %s, not the image fleet delivered", podspec.JobContainer, st.ImageID))
		}
	}
	if len(reasons) > 0 {
		return "", &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: reasons}
	}
	return learned, nil
}

// learnNodeImage checks the instance's first pod's image: one the record
// holds, or, for a node-loaded image whose id the seed's pod did not show,
// the id the node gave it, learned (as checkJobPod's).
func (c *creation) learnNodeImage(pod *corev1.Pod, rec *InstanceRecord, del *kubeimage.Delivery) error {
	st := containerStatus(pod, DevContainer)
	if st == nil || st.ImageID == "" || digestIn(st.ImageID, rec.ImageDigests) {
		return nil
	}
	if del.Strategy != kubeimage.StrategyNodeLoad || c.nodeImage != "" {
		return &TamperedError{Namespace: pod.Namespace, Pod: pod.Name, Reasons: []string{fmt.Sprintf("its %s container runs %s, not the image fleet delivered", DevContainer, st.ImageID)}}
	}
	rec.addImageDigest(imageDigest(st.ImageID))
	return SaveInstanceRecord(*rec)
}

// failedCreate is WaitReady's check: a FailedCreate event of the
// StatefulSet since the create began, classified (admission changed since
// the dry-run, a quota filled meanwhile).
func (c *creation) failedCreate(sts string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		evs, err := c.cs.CoreV1().Events(c.ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=StatefulSet,involvedObject.name=" + sts + ",reason=FailedCreate"})
		if err != nil {
			return nil // events are a hint; the deadlines still bound the wait
		}
		for _, ev := range evs.Items {
			if eventTime(ev).Before(c.start) {
				continue
			}
			if d := podspec.ClassifyFailedCreate("StatefulSet "+sts, ev.Message); d != nil {
				return d
			}
		}
		return nil
	}
}

func eventTime(ev corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// warn reports a warning to the job's log and keeps it for the banner.
func (c *creation) warn(ctx context.Context, w string) {
	c.warnings = append(c.warnings, w)
	backend.ReportWarning(ctx, w)
}

// warningLines are the create's warnings for the instance's banner
// (UpResult.Warnings, which the create job puts there after its own
// provisioning steps), each once.
func (c *creation) warningLines() []string {
	seen := map[string]bool{}
	var lines []string
	for _, w := range c.warnings {
		if w = strings.TrimSpace(w); w != "" && !seen[w] {
			seen[w] = true
			lines = append(lines, w)
		}
	}
	return lines
}

// finish ends the job's journal, whatever the outcome: a failed create's
// record is failed by the create job, its objects stay labelled by the key
// for a destroy. Only a daemon that dies or shuts down mid-create leaves a
// journal behind (checkpoint).
func (c *creation) finish() {
	if c.checkpointed {
		return
	}
	ctx, cancel := c.cleanupContext()
	defer cancel()
	if s, err := c.scope(); err == nil {
		if err := s.ClearMark(ctx, c.jj.Journal()); err != nil {
			flog.Warn("kube create: clearing the job's mark", "cluster", c.id.cluster, "key", c.id.key, "err", err)
		}
	}
	if err := c.jj.Finish(); err != nil {
		flog.Warn("kube create: removing the job's journal", "cluster", c.id.cluster, "key", c.id.key, "err", err)
	}
}

// stopFailed scales a failed create's pod to 0: the instance is FAILED, and
// its disk stays for a destroy (or a retry) to deal with.
func (c *creation) stopFailed() {
	if !c.started {
		return
	}
	ctx, cancel := c.cleanupContext()
	defer cancel()
	if err := c.p.Scale(ctx, c.id.cluster, c.id.sts, 0); err != nil && !gone(err) {
		flog.Warn("kube create: scaling the failed instance down", "cluster", c.id.cluster, "sts", c.id.sts, "err", err)
	}
}

// instanceIdentity is whom an instance's containers run as: as root, as
// docker's runtime does, unless admission refuses root pods, when every
// container runs as the share uid (capability tier C).
func instanceIdentity(caps *fleetgrpc.KubeCapabilities) podspec.Identity {
	if a := caps.GetAdmission(); a == nil || a.GetUnprivileged() {
		return podspec.Identity{}
	}
	uid, gid := shareIDs()
	u, g := int64(uid), int64(gid)
	return podspec.Identity{NonRoot: true, UID: &u, GID: &g}
}

// shareIDs are the uid and gid the remote user is remapped to and the
// workspace is owned by (podspec.ShareIDs).
func shareIDs() (int, int) { return podspec.ShareIDs() }

// clusterFacts is the translation's view of the preflight's findings, for
// an instance disk of StorageClass class ("" is the cluster's default).
func clusterFacts(f Facts, class string) podspec.ClusterFacts {
	caps := f.Capabilities
	out := podspec.ClusterFacts{
		Local:                 caps.GetLocalCluster() || f.Local.Local(),
		LimitRangeMemoryLimit: caps.GetLimitRangeMemoryLimit(),
		RuntimeClasses:        caps.GetRuntimeClasses(),
		ServerVersion:         caps.GetVersion(),
	}
	for _, n := range caps.GetNodes() {
		out.Nodes = append(out.Nodes, podspec.Node{Arch: n.GetArch(), AllocatableCPU: n.GetAllocatableCpu(), AllocatableMemory: n.GetAllocatableMemory()})
	}
	for _, sc := range caps.GetStorageClasses() {
		if (class != "" && sc.GetName() == class) || (class == "" && sc.GetIsDefault()) {
			if !sc.GetBlock() {
				out.DiskFileShare = sc.GetName()
			}
			break
		}
	}
	return out
}
