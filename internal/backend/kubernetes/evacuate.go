//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// evacuate.go moves a fleet in single-node mode off a node the cluster is
// emptying (a cordon, a drain, a node upgrade, the autoscaler or Karpenter
// removing it; nodes.go sees it). Every pod of such a fleet is required
// beside the fleet's running pods, so none can leave on its own: a pod the
// drain evicts comes back on the same node (cordoned: it never starts), and
// with protect_from_drain the drain waits forever. So the daemon moves the
// fleet as one:
//
//  1. Each running instance's StatefulSet is marked (AnnotationMovingOff),
//     so a daemon that restarts mid-move finishes it.
//  2. Every one of them is stopped as a Fleet stop does (scaled to 0, its
//     pod gone).
//  3. The fleet's volumes (its share and the instances' disks) are waited
//     for to detach from the node (VolumeAttachments), as a disk that
//     attaches to one node at a time must before a pod elsewhere mounts it.
//  4. They are all started together, as a Fleet start does: the scheduler
//     puts the first wherever the share's topology allows (its zone), and
//     the rest beside it.
//  5. The marks go.
//
// The cluster restarted each instance's processes as a reboot would, so its
// row says "sessions reset: fleet moved" (Pool.ExpectReplacement). A fleet
// whose volumes are on the node's own disk (a node-local StorageClass), or
// whose pods are required on that node by name, cannot move: nothing is
// stopped, and the reason is reported (Blocked).

// Evacuation bounds.
var (
	// evacuationDetachWait bounds the wait for the fleet's volumes to leave
	// the node: the attach/detach controller force-detaches after 6 minutes.
	evacuationDetachWait = 6 * time.Minute
	evacuationDetachPoll = 2 * time.Second
	// evacuationDrainGap is how long an instance protected from drains (its
	// PodDisruptionBudget refuses every eviction) stays stopped before it
	// starts again: kubectl drain asks to evict a refused pod again by name
	// every 5 seconds, and a StatefulSet's new pod has the old one's name,
	// so started at once it would be what the drain asks for next, refused
	// until the drain times out. Stopped past one retry, the drain finds no
	// pod and is done with it.
	evacuationDrainGap = 8 * time.Second
)

// Evacuation is one fleet's move off a node of a connection.
type Evacuation struct {
	Cluster string
	// Node is the node being emptied, Why why (NodeDisruption).
	Node, Why string
	// Fleet names the fleet (for the log); Share is its share PVC.
	Fleet, Share string
	// Members are the fleet's running instances on the connection.
	Members []EvacuationMember
}

// EvacuationMember is one instance an evacuation moves.
type EvacuationMember struct {
	Instance string
	// StatefulSet is the instance's (its disk PVC has its name).
	StatefulSet string
	// Stop and Start stop and start the instance as a Fleet stop and start
	// do (its backend handle's, then fleet's own post-start work).
	Stop  func() error
	Start func() error
	// Wanted reports, just before the instance is started again, whether it
	// still is to run (a Fleet stop or a destroy meanwhile says no); nil:
	// always.
	Wanted func() bool
}

// EvacuationOutcome is what a move did to one instance.
type EvacuationOutcome struct {
	Instance string
	// Stopped: the move stopped it. Started: it runs again. Skipped: it was
	// stopped or destroyed meanwhile, so the move left it stopped. Err says
	// why it did not stop or start.
	Stopped, Started, Skipped bool
	Err                       error
}

// EvacuationResult is what a move did.
type EvacuationResult struct {
	// Blocked says why the fleet cannot leave the node ("" when it could):
	// nothing was stopped.
	Blocked  string
	Outcomes []EvacuationOutcome
	// To is the node the fleet runs on afterwards ("" when none of it runs).
	To string
}

// Evacuate moves a fleet off a node (see the file comment). An error is a
// move that could not begin (the connection is unusable), or one ctx ended
// before the fleet was started again (its marks stay for the next daemon);
// what happened to each instance is in the result.
func (p *Pool) Evacuate(ctx context.Context, e Evacuation) (EvacuationResult, error) {
	c, err := p.Cluster(e.Cluster)
	if err != nil {
		return EvacuationResult{}, classify(err)
	}
	cs, err := p.clientset(c)
	if err != nil {
		return EvacuationResult{}, classify(err)
	}
	ns := c.Namespace()
	log := p.opts.Logger.With("cluster", e.Cluster, "fleet", e.Fleet, "node", e.Node)
	if why := p.moveBlocked(ctx, cs, ns, e); why != "" {
		log.Warn("kube evacuate: the fleet cannot move off the node the cluster empties", "why", e.Why, "blocked", why)
		return EvacuationResult{Blocked: why}, nil
	}
	start := p.opts.Now()
	res := EvacuationResult{Outcomes: make([]EvacuationOutcome, len(e.Members))}
	insts := make([]string, len(e.Members))
	for i, m := range e.Members {
		res.Outcomes[i].Instance = m.Instance
		insts[i] = m.Instance
	}
	log.Warn("kube evacuate: moving the fleet off a node the cluster empties", "why", e.Why, "instances", strings.Join(insts, ","))

	for _, m := range e.Members {
		if err := markMoving(ctx, cs, ns, m.StatefulSet, e.Node); err != nil && !apierrors.IsNotFound(err) {
			// The mark only lets a daemon that restarts mid-move finish it.
			log.Warn("kube evacuate: marking the instance as moving", "instance", m.Instance, "err", err)
		}
	}
	protected := p.protectedFromDrains(ctx, cs, ns, e.Members)
	each(e.Members, func(i int, m EvacuationMember) {
		o := &res.Outcomes[i]
		if err := m.Stop(); err != nil {
			o.Err = fmt.Errorf("stopping it: %w", err)
			log.Error("kube evacuate: stopping an instance failed", "instance", m.Instance, "err", err)
			return
		}
		o.Stopped = true
	})
	stopped := p.opts.Now()
	// The disks of the instances that stopped, and the share once none of
	// its pods is left (one that did not stop keeps it attached).
	claims := []string{}
	all := true
	for i, m := range e.Members {
		if res.Outcomes[i].Stopped {
			claims = append(claims, m.StatefulSet)
		} else {
			all = false
		}
	}
	if e.Share != "" && all {
		claims = append(claims, e.Share)
	}
	if err := p.waitDetached(ctx, cs, ns, e.Node, claims); err != nil && ctx.Err() == nil {
		// The kubelet retries the attach; the start's own deadline bounds it.
		log.Warn("kube evacuate: the fleet's volumes did not leave the node in time; starting it anyway", "err", err)
	}
	if protected {
		if gap := evacuationDrainGap - p.opts.Now().Sub(stopped); gap > 0 {
			_ = sleepCtx(ctx, gap)
		}
	}
	if err := ctx.Err(); err != nil {
		// The daemon is stopping: the marks stay, and the next one starts
		// the fleet again.
		log.Warn("kube evacuate: the daemon stopped before the fleet was started again; the next daemon finishes the move")
		return res, err
	}
	each(e.Members, func(i int, m EvacuationMember) {
		o := &res.Outcomes[i]
		if o.Err != nil {
			return
		}
		if m.Wanted != nil && !m.Wanted() {
			o.Skipped = true
			return
		}
		p.ExpectReplacement(e.Cluster, m.StatefulSet, backend.RestartFleetMoved)
		if err := m.Start(); err != nil {
			o.Err = fmt.Errorf("starting it again: %w", err)
			log.Error("kube evacuate: starting an instance again failed", "instance", m.Instance, "err", err)
			return
		}
		o.Started = true
	})
	for _, m := range e.Members {
		if err := markMoving(ctx, cs, ns, m.StatefulSet, ""); err != nil && !apierrors.IsNotFound(err) {
			log.Warn("kube evacuate: clearing the instance's moving mark", "instance", m.Instance, "err", err)
		}
	}
	for i, o := range res.Outcomes {
		if !o.Started {
			continue
		}
		if pod, err := cs.CoreV1().Pods(ns).Get(ctx, names.Pod(e.Members[i].StatefulSet), metav1.GetOptions{}); err == nil && pod.Spec.NodeName != "" {
			res.To = pod.Spec.NodeName
			break
		}
	}
	log.Info("kube evacuate: moved the fleet", "to", res.To, "ms", p.opts.Now().Sub(start).Milliseconds())
	return res, nil
}

// each runs fn on every member at once and waits for them all.
func each(ms []EvacuationMember, fn func(int, EvacuationMember)) {
	var wg sync.WaitGroup
	for i, m := range ms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(i, m)
		}()
	}
	wg.Wait()
}

// protectedFromDrains reports whether one of the members has a
// PodDisruptionBudget (protect_from_drain; it has the instance's name).
func (p *Pool) protectedFromDrains(ctx context.Context, cs kclient.Interface, ns string, ms []EvacuationMember) bool {
	for _, m := range ms {
		if _, err := cs.PolicyV1().PodDisruptionBudgets(ns).Get(ctx, m.StatefulSet, metav1.GetOptions{}); err == nil {
			return true
		}
	}
	return false
}

// ClearMovingOff removes the instance's AnnotationMovingOff, the mark of a
// move that is over though the mark stayed (the daemon that made the move
// stopped while starting the fleet again, or could not remove it): left, it
// would make the instance's next pod that is not running a move of its
// fleet.
func (p *Pool) ClearMovingOff(ctx context.Context, clusterID, sts string) error {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return classify(err)
	}
	cs, err := p.clientset(c)
	if err != nil {
		return classify(err)
	}
	if err := markMoving(ctx, cs, c.Namespace(), sts, ""); err != nil && !apierrors.IsNotFound(err) {
		return classify(err)
	}
	return nil
}

// markMoving sets (node "" removes) the StatefulSet's AnnotationMovingOff.
func markMoving(ctx context.Context, cs kclient.Interface, ns, sts, node string) error {
	var v any = node
	if node == "" {
		v = nil
	}
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{names.AnnotationMovingOff: v}}})
	_, err := cs.AppsV1().StatefulSets(ns).Patch(ctx, sts, types.MergePatchType, body, metav1.PatchOptions{FieldManager: DefaultFieldManager})
	return err
}

// moveBlocked says, in a few words, why the fleet cannot leave node ("" when
// it can): one of its volumes is on the node's own disk ("its share is
// node-local, class standard"), or its pods are required on the node by name
// ("its pods are pinned to the node"). A volume is judged by its own node
// affinity when it is readable (a PersistentVolume is cluster-scoped), and
// by its StorageClass as the connection's last preflight found it.
func (p *Pool) moveBlocked(ctx context.Context, cs kclient.Interface, ns string, e Evacuation) string {
	var nodeLocal map[string]bool
	classLocal := func(class string) bool {
		if nodeLocal == nil {
			nodeLocal = map[string]bool{}
			if f := currentEnvironment().Facts; f != nil {
				fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				facts, err := f(fctx, e.Cluster)
				cancel()
				if err == nil {
					for _, sc := range facts.Capabilities.GetStorageClasses() {
						nodeLocal[sc.GetName()] = sc.GetNodeLocal()
						if sc.GetIsDefault() {
							nodeLocal[""] = sc.GetNodeLocal()
						}
					}
				}
			}
		}
		return nodeLocal[class]
	}
	check := func(claim, what string) string {
		pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		class := ""
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		if name := pvc.Spec.VolumeName; name != "" {
			pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
			if err == nil && pinnedTo(topology.VolumeTerms(pv), e.Node) {
				return fmt.Sprintf("%s is node-local, class %s", what, orDefault(class, "default"))
			}
		}
		// Also for a readable volume: a node-local class may name its node by
		// a key of its own (csi-hostpath, TopoLVM) or not at all (minikube's
		// hostpath), and a pod started elsewhere would wait for the node, or
		// find an empty folder.
		if classLocal(class) {
			return fmt.Sprintf("%s is node-local, class %s", what, orDefault(class, "default"))
		}
		return ""
	}
	if e.Share != "" {
		if why := check(e.Share, "its share"); why != "" {
			return why
		}
	}
	for _, m := range e.Members {
		if why := check(m.StatefulSet, m.Instance+"'s disk"); why != "" {
			return why
		}
	}
	// The share's recorded topology names the node (its volume was not
	// readable when the share was placed).
	for _, m := range e.Members {
		set, err := cs.AppsV1().StatefulSets(ns).Get(ctx, m.StatefulSet, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if a := set.Spec.Template.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil &&
			pinnedTo(a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, e.Node) {
			return "its pods are pinned to the node"
		}
	}
	return ""
}

// pinnedTo reports whether terms (any one of them may be met) allow node
// and no other: every term names the node by hostname (or by name) alone.
func pinnedTo(terms []corev1.NodeSelectorTerm, node string) bool {
	if len(terms) == 0 {
		return false
	}
	only := func(op corev1.NodeSelectorOperator, values []string) bool {
		if op != corev1.NodeSelectorOpIn || len(values) == 0 {
			return false
		}
		for _, v := range values {
			if v != node {
				return false
			}
		}
		return true
	}
	for _, t := range terms {
		pinned := false
		for _, r := range t.MatchExpressions {
			if r.Key == corev1.LabelHostname && only(r.Operator, r.Values) {
				pinned = true
			}
		}
		for _, r := range t.MatchFields {
			if r.Key == "metadata.name" && only(r.Operator, r.Values) {
				pinned = true
			}
		}
		if !pinned {
			return false
		}
	}
	return true
}

// waitDetached waits until none of claims' volumes is attached to node
// (their VolumeAttachments are gone), within evacuationDetachWait. A volume
// that never attaches (a network filesystem, a node-local disk) has none.
// VolumeAttachments are cluster-scoped: a connection that may not list them
// does not wait.
func (p *Pool) waitDetached(ctx context.Context, cs kclient.Interface, ns, node string, claims []string) error {
	pvs := map[string]bool{}
	for _, claim := range claims {
		if pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{}); err == nil && pvc.Spec.VolumeName != "" {
			pvs[pvc.Spec.VolumeName] = true
		}
	}
	if len(pvs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, evacuationDetachWait)
	defer cancel()
	for {
		list, err := cs.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsForbidden(err):
			p.opts.Logger.Debug("kube evacuate: VolumeAttachments are not readable; not waiting for the volumes to detach", "node", node)
			return nil
		case err == nil:
			attached := []string{}
			for _, va := range list.Items {
				if pv := va.Spec.Source.PersistentVolumeName; pv != nil && pvs[*pv] && va.Spec.NodeName == node {
					attached = append(attached, *pv)
				}
			}
			if len(attached) == 0 {
				return nil
			}
		case ctx.Err() == nil:
			p.opts.Logger.Debug("kube evacuate: listing VolumeAttachments", "err", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the fleet's volumes were still attached to node %s after %s", node, evacuationDetachWait)
		case <-time.After(evacuationDetachPoll):
		}
	}
}
