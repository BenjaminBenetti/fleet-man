//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// nodes.go watches the nodes that hold a fleet's share in single-node mode
// (the nodes its instance pods run on), so the daemon learns at once when
// the cluster starts emptying one: a cordon (kubectl cordon, the first step
// of kubectl drain and of a node upgrade), or the cluster autoscaler or
// Karpenter about to remove it. Such a fleet cannot follow its pods off the
// node on its own (each pod is required beside the running ones, which are
// there), so the daemon moves it (evacuate.go).
//
// One informer per watched node, narrowed to it by name: a fleet's pods are
// on one node, so a daemon watches a few nodes, not the cluster's whole list
// (which is large, and changes all the time on a big cluster). It runs while
// a holder pod of the daemon's is on the node and stops when the last one
// leaves. Nodes are cluster-scoped: a connection that may not list or watch
// them (fleet's Role grants none; a ClusterRole is needed) still runs, but
// its fleets are not moved off a cordoned node; fleet.log says so once.

// Taints that mark a node the cluster is emptying (beside spec.unschedulable,
// a cordon).
const (
	// taintAutoscalerDelete: the cluster autoscaler is removing the node.
	taintAutoscalerDelete = "ToBeDeletedByClusterAutoscaler"
	// taintKarpenterDisrupted (Karpenter v1) and taintKarpenterDisruption
	// with value "disrupting" (v1beta1): Karpenter is replacing the node.
	taintKarpenterDisrupted  = "karpenter.sh/disrupted"
	taintKarpenterDisruption = "karpenter.sh/disruption"
)

// NodeDisruption says why the cluster is emptying node n, in the words a
// status line uses ("cordoned", "being removed by the cluster autoscaler",
// "being replaced by Karpenter"); "" when it is not.
func NodeDisruption(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	if n.Spec.Unschedulable {
		return "cordoned"
	}
	for _, t := range n.Spec.Taints {
		switch {
		case t.Key == corev1.TaintNodeUnschedulable:
			return "cordoned"
		case t.Key == taintAutoscalerDelete:
			return "being removed by the cluster autoscaler"
		case t.Key == taintKarpenterDisrupted, t.Key == taintKarpenterDisruption && t.Value == "disrupting":
			return "being replaced by Karpenter"
		}
	}
	return ""
}

// singleNodeHolder reports whether pod holds its fleet's share in
// single-node mode: an instance pod required beside the running pods of its
// share group (topology.Placement.Affinity gives only those that affinity).
func singleNodeHolder(pod *corev1.Pod) bool {
	if pod == nil || pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil {
		return false
	}
	group := pod.Labels[names.LabelShareGroup]
	if group == "" || pod.Labels[names.LabelRole] != string(names.RoleInstance) {
		return false
	}
	want := topology.HolderLabels(group)
	for _, term := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey != corev1.LabelHostname || term.LabelSelector == nil {
			continue
		}
		match := term.LabelSelector.MatchLabels
		if len(match) == len(want) && match[names.LabelShareGroup] == want[names.LabelShareGroup] && match[names.LabelRole] == want[names.LabelRole] {
			return true
		}
	}
	return false
}

// holderNode is the node a single-node holder pod is on ("" for any other
// pod, one not scheduled yet, and one that is gone).
func holderNode(pod *corev1.Pod, deleted bool) string {
	if deleted || !singleNodeHolder(pod) {
		return ""
	}
	return pod.Spec.NodeName
}

// nodeWatches are one connection's node informers.
type nodeWatches struct {
	byNode map[string]*nodeWatch
	// refused: the API server refused a node list (logged once, until a
	// list works again).
	refused bool
}

// nodeWatch is one node's informer and what it last showed.
type nodeWatch struct {
	node   string
	cancel context.CancelFunc

	mu sync.Mutex
	// disruption is NodeDisruption of the node as last seen ("" also before
	// the first list, and once it is deleted).
	disruption string
}

func (w *nodeWatch) current() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.disruption
}

// syncNodeWatches starts a watch of every node a single-node holder of the
// connection is on and stops those no holder is on any more.
func (p *Pool) syncNodeWatches(clusterID string) {
	want := map[string]bool{}
	p.instMu.Lock()
	for k, t := range p.tracks {
		if k.cluster == clusterID && t.holderNode != "" {
			want[t.holderNode] = true
		}
	}
	p.instMu.Unlock()

	var start []string
	var stop []*nodeWatch
	p.nodeMu.Lock()
	ws := p.nodes[clusterID]
	if ws == nil {
		ws = &nodeWatches{byNode: map[string]*nodeWatch{}}
		p.nodes[clusterID] = ws
	}
	for node, w := range ws.byNode {
		if !want[node] {
			stop = append(stop, w)
			delete(ws.byNode, node)
		}
	}
	for node := range want {
		if ws.byNode[node] == nil {
			start = append(start, node)
		}
	}
	sort.Strings(start)
	p.nodeMu.Unlock()
	for _, w := range stop {
		w.cancel()
	}
	for _, node := range start {
		p.startNodeWatch(clusterID, node)
	}
}

// startNodeWatch starts the informer of one node of a connection.
func (p *Pool) startNodeWatch(clusterID, node string) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return
	}
	cs, err := p.clientset(c)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	w := &nodeWatch{node: node, cancel: cancel}
	p.nodeMu.Lock()
	ws := p.nodes[clusterID]
	if ws == nil || ws.byNode[node] != nil {
		// Removed meanwhile, or another caller started one.
		p.nodeMu.Unlock()
		cancel()
		return
	}
	ws.byNode[node] = w
	p.nodeMu.Unlock()

	sel := fields.OneTermEqualSelector("metadata.name", node).String()
	nodes := cs.CoreV1().Nodes()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.FieldSelector = sel
			obj, err := nodes.List(ctx, o)
			p.nodesAnswered(ctx, clusterID, err)
			return obj, err
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = sel
			return nodes.Watch(ctx, o)
		},
	}
	inf := cache.NewSharedIndexInformerWithOptions(cache.ToListWatcherWithWatchListSemantics(lw, cs), &corev1.Node{}, cache.SharedIndexInformerOptions{ObjectDescription: "node " + node})
	// A node's status (its images, its conditions) is most of it and changes
	// all the time; only its name, unschedulable and taints are kept.
	_ = inf.SetTransform(slimNode)
	_ = inf.SetWatchErrorHandlerWithContext(func(ctx context.Context, _ *cache.Reflector, err error) {
		switch {
		case err == nil, errors.Is(err, io.EOF), apierrors.IsResourceExpired(err), apierrors.IsGone(err):
			return
		}
		p.nodesAnswered(ctx, clusterID, err)
	})
	seen := func(obj any, deleted bool) {
		if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj, deleted = tomb.Obj, true
		}
		n, ok := obj.(*corev1.Node)
		if !ok || n.Name != node {
			return
		}
		why := ""
		if !deleted {
			why = NodeDisruption(n)
		}
		w.mu.Lock()
		changed := why != w.disruption
		w.disruption = why
		w.mu.Unlock()
		if changed {
			if why != "" {
				p.opts.Logger.Warn("kube nodes: a node holding a fleet is being emptied", "cluster", clusterID, "node", node, "why", why)
			}
			p.nodeChanged(clusterID, node)
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { seen(obj, false) },
		UpdateFunc: func(_, obj any) { seen(obj, false) },
		DeleteFunc: func(obj any) { seen(obj, true) },
	}); err != nil {
		cancel()
		return
	}
	go inf.RunWithContext(ctx)
}

// slimNode is the part of a node the watch keeps.
func slimNode(obj any) (any, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.Name, UID: n.UID, ResourceVersion: n.ResourceVersion},
		Spec:       corev1.NodeSpec{Unschedulable: n.Spec.Unschedulable, Taints: n.Spec.Taints},
	}, nil
}

// nodesAnswered records whether the API server let a connection list its
// nodes, logging a refusal once.
func (p *Pool) nodesAnswered(ctx context.Context, clusterID string, err error) {
	if ctx.Err() != nil {
		return
	}
	refused := err != nil && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err))
	p.nodeMu.Lock()
	ws := p.nodes[clusterID]
	was := ws != nil && ws.refused
	if ws != nil && (refused || err == nil) {
		ws.refused = refused
	}
	p.nodeMu.Unlock()
	if refused && !was {
		p.opts.Logger.Warn("kube nodes: the connection may not watch nodes, so a fleet on one node is not moved off a node the cluster empties (grant get, list and watch on nodes)", "cluster", clusterID, "err", err)
	}
}

// nodeChanged tells about every instance whose pod is on the node.
func (p *Pool) nodeChanged(clusterID, node string) {
	var keys []trackKey
	p.instMu.Lock()
	for k, t := range p.tracks {
		if k.cluster == clusterID && t.pod != nil && t.pod.Spec.NodeName == node {
			keys = append(keys, k)
		}
	}
	p.instMu.Unlock()
	for _, k := range keys {
		p.changed(k)
	}
}

// NodeDisruptionOf is why the cluster is emptying node, as the connection's
// watch of it last showed ("" when it is not, and for a node not watched:
// no single-node holder of the daemon's is on it).
func (p *Pool) NodeDisruptionOf(clusterID, node string) string {
	p.nodeMu.Lock()
	defer p.nodeMu.Unlock()
	if ws := p.nodes[clusterID]; ws != nil {
		if w := ws.byNode[node]; w != nil {
			return w.current()
		}
	}
	return ""
}

// stopNodeWatchesLocked stops a removed connection's node watches (p.nodeMu
// held).
func (p *Pool) stopNodeWatchesLocked(clusterID string) {
	if ws := p.nodes[clusterID]; ws != nil {
		for _, w := range ws.byNode {
			w.cancel()
		}
		delete(p.nodes, clusterID)
	}
}
