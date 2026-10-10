//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// lifecycle.go is an instance's stop, start and teardown. The instance disk
// is docker's writable layer, so a stop is a scale to 0 (the pod goes, the
// disk stays) and a start a scale to 1, after which the pod is verified, its
// agent connected (and restaged to the daemon's build of fleet when it runs
// another) and the devcontainer's start hooks run. A teardown deletes every
// object of the instance's key, so it reaches what a ContainerID never names
// (a failed create's objects, the seed Job, older Secrets).

// Bounds of the lifecycle steps (their waits have their own deadlines).
const (
	stopTimeout      = 3 * time.Minute
	startTimeout     = 30 * time.Minute
	downTimeout      = 3 * time.Minute
	postStartTimeout = 10 * time.Minute
)

// Stop scales the instance to 0 and waits for its pod to be gone, as docker
// stop waits for the container to stop. The disk keeps the instance.
func (b *Backend) Stop(containerID string) error {
	w, err := b.where(containerID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	p := b.p()
	if err := p.Scale(ctx, w.cluster, w.sts, 0); err != nil {
		return classify(err)
	}
	s, err := p.Scope(w.cluster)
	if err != nil {
		return classify(err)
	}
	return classify(p.waitPodGone(ctx, s, names.Pod(w.sts)))
}

// Start scales the instance to 1, waits for its pod (delivering its image
// again when the node lost it, or warming the gateway a gateway-delivered
// one pulls through), connects its agent (restaging the daemon's
// fleet build into it) and runs the devcontainer's start hooks
// (postStart, postAttach). A hook that fails is a warning, as on docker: the
// instance runs. A pod that does not come up, or whose agent does not
// connect, fails the start, scaled back to 0.
func (b *Backend) Start(containerID string) error {
	w, err := b.where(containerID)
	if err != nil {
		return err
	}
	id, err := b.identity()
	if err != nil {
		return err
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	p := b.p()
	warmGateway(kubeimage.Strategy(rec.Strategy))
	if err := p.Scale(ctx, w.cluster, w.sts, 1); err != nil {
		return classify(err)
	}
	// A start that fails leaves the instance stopped, as a docker container
	// that does not start is: its pod is not left to crash loop, or to wait
	// on its image, behind a record that says stopped.
	failed := func(err error) error {
		down, cancel := context.WithTimeout(context.Background(), finishTimeout)
		defer cancel()
		if serr := p.Scale(down, w.cluster, w.sts, 0); serr != nil {
			flog.Warn("kube start: scaling the instance that did not start back down", "cluster", w.cluster, "sts", w.sts, "err", serr)
		}
		return classify(err)
	}
	redelivered := false
	pulls := newGatewayPulls(p, kubeimage.Strategy(rec.Strategy), w.cluster, w.sts)
	pod, err := p.WaitReady(ctx, w.cluster, w.sts, WaitOptions{PVC: w.sts, Check: func(ctx context.Context) error {
		if !redelivered && b.redeliver(ctx, rec) {
			redelivered = true
		}
		return pulls.check(ctx)
	}})
	if err != nil {
		return failed(pulls.explain(err))
	}
	t := rec.target()
	t.Priority, t.Restage = PriorityInteractive, true
	sess, err := p.EnsureAgent(ctx, t)
	if err != nil {
		return failed(err)
	}
	if err := b.startHooks(ctx, sess, pod, rec, containerID); err != nil {
		state.WriteWarn(id.fleet, id.name, fmt.Sprintf("lifecycle hooks after start: %v", err))
	}
	return nil
}

// RunPostStart runs the devcontainer's start hooks again in an instance the
// cluster restarted on its own (backend.PostStarter), bounded.
func (b *Backend) RunPostStart(containerID string) error {
	id, err := b.identity()
	if err != nil {
		return err
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postStartTimeout)
	defer cancel()
	sess, err := b.agent(ctx, false)
	if err != nil {
		return err
	}
	pod, err := b.p().Verify(ctx, rec.target())
	if err != nil {
		return classify(err)
	}
	return b.startHooks(ctx, sess, pod, rec, containerID)
}

// startHooks runs set-up for a start: the creation hooks have their markers
// already, so postStart (once per run of the dev container) and postAttach
// run.
func (b *Backend) startHooks(ctx context.Context, sess *AgentSession, pod *corev1.Pod, rec InstanceRecord, containerID string) error {
	if containerID == "" {
		containerID = b.inst.ContainerID
	}
	container, err := b.p().inspectedContainer(ctx, rec.Cluster, rec.StatefulSet, rec.Gen)
	if err != nil {
		return err
	}
	return setUp{
		session: sess, pod: pod, container: container, containerID: containerID,
		created: rec.Created, workspaceFolder: rec.WorkspaceFolder, tools: currentEnvironment().Tools,
	}.run(ctx)
}

// redeliver delivers the instance's image again when its pod waits on an
// image its node no longer has (a node-loaded image the node's image GC
// collected, a registry image deleted meanwhile); the kubelet then starts the
// pod on its own. Only fleetd can (the delivery needs the connection's facts).
// It reports whether it delivered.
func (b *Backend) redeliver(ctx context.Context, rec InstanceRecord) bool {
	env := currentEnvironment()
	if env.Facts == nil {
		return false
	}
	p := b.p()
	c, err := p.Cluster(rec.Cluster)
	if err != nil {
		return false
	}
	objs, err := p.objects(c)
	if err != nil {
		return false
	}
	pod, err := objs.Pod(ctx, c.Namespace(), names.Pod(rec.StatefulSet))
	if err != nil {
		return false
	}
	st := containerStatus(pod, DevContainer)
	if st == nil || st.State.Waiting == nil || !kubeimage.NeedsRedelivery(kubeimage.Strategy(rec.Strategy), st.State.Waiting.Reason, st.State.Waiting.Message) {
		return false
	}
	images := env.images()
	d, err := images.Lookup(rec.Cluster, rec.Key)
	if err != nil || d == nil {
		flog.Warn("kube start: the instance's image is gone from its node, and fleet has no delivery of it to repeat", "cluster", rec.Cluster, "key", rec.Key, "err", err)
		return false
	}
	t, err := b.imageTarget(ctx, c)
	if err != nil {
		flog.Warn("kube start: re-delivering the instance's image", "cluster", rec.Cluster, "key", rec.Key, "err", err)
		return false
	}
	if _, err := images.Redeliver(ctx, t, *d); err != nil {
		flog.Warn("kube start: re-delivering the instance's image", "cluster", rec.Cluster, "key", rec.Key, "err", err)
		return false
	}
	flog.Info("kube start: delivered the instance's image again", "cluster", rec.Cluster, "key", rec.Key, "reason", st.State.Waiting.Reason)
	return true
}

// Down tears the instance down by its key (every object of it), whatever
// generation containerID names.
func (b *Backend) Down(string) error { return b.teardown() }

// DownByKey tears down an instance that has no ContainerID (a create that
// failed or was interrupted after its first apply): backend.OrphanDowner. A
// record without a key has nothing in any cluster.
func (b *Backend) DownByKey() error {
	if b.inst.KubeKey == "" {
		return nil
	}
	return b.teardown()
}

// teardown is a destroy's: the key's tombstone first (whatever the teardown
// cannot delete now is known garbage), every object of the key, the image
// released, fleetd's record of the instance and the pool's state of its pod
// dropped. A teardown the cluster refuses (unreachable, another daemon owns
// it) fails, but the destroy goes on and removes the instance's state record
// all the same, so the image hold and fleetd's record go with it: the
// tombstone is what finishes the objects later.
func (b *Backend) teardown() error {
	if b.inst.KubeCluster == "" || !names.ValidHash(b.inst.KubeKey) {
		return errUnbound
	}
	fleetName, _ := b.fleet()
	cluster, key := b.inst.KubeCluster, b.inst.KubeKey
	ctx, cancel := context.WithTimeout(context.Background(), downTimeout)
	defer cancel()
	p := b.p()
	td, err := p.DownByKey(ctx, Tombstone{Cluster: cluster, Key: key, Fleet: fleetName, Instance: b.inst.Name, DestroyedAt: time.Now().UTC()})
	if fleetName != "" {
		p.Forget(PodTarget{Cluster: cluster, StatefulSet: names.Instance(fleetName, b.inst.Name, key)})
	}
	if rerr := currentEnvironment().images().Release(cluster, key); rerr != nil {
		flog.Warn("kube down: releasing the instance's image", "cluster", cluster, "key", key, "err", rerr)
	}
	if rerr := RemoveInstanceRecord(cluster, key); rerr != nil {
		flog.Warn("kube down: removing the instance's record", "cluster", cluster, "key", key, "err", rerr)
	}
	if fleetName != "" {
		RemoveContainerLog(fleetName, b.inst.Name)
	}
	if err != nil {
		return classify(err)
	}
	flog.Info("kube down: the instance's objects are deleted", "cluster", cluster, "key", key, "deleted", len(td.Deleted), "still_going", len(td.Remaining))
	return nil
}
