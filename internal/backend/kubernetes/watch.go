//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// watch.go runs each connection's informers: the pods and StatefulSets of
// this daemon's instances in the connection's namespace (its owner,
// connection and role=instance labels). They start the first time an
// instance of the connection is tracked (Track) and run until the pool
// closes or the connection is reloaded or removed. Everything fleetd knows of
// its instance pods between API calls comes from them: verification reads
// (Objects), Observe, the restart and crash-loop tracking (track.go) and the
// supervisor's pokes (supervisor.go). No periodic resync: an unchanged pod
// costs nothing.

// instanceSelector selects the instance pods and StatefulSets this daemon
// owns on one connection (never a Job's pod, whose role differs).
func instanceSelector(owner, clusterID string) string {
	return names.Selector(owner, clusterID) + "," + names.LabelRole + "=" + string(names.RoleInstance)
}

// watcher is one connection's informers.
type watcher struct {
	pool    *Pool
	cluster *Cluster
	ns      string
	match   labels.Selector
	pods    cache.SharedIndexInformer
	sets    cache.SharedIndexInformer
	cancel  context.CancelFunc
	done    chan struct{}
	synced  chan struct{}

	mu sync.Mutex
	// err is the last list or watch failure, nil once a list or watch
	// succeeds again.
	err error
}

// startWatcher starts c's informers, or says why they cannot run (the
// daemon's identity is unreadable, the client does not build).
func (p *Pool) startWatcher(c *Cluster) (*watcher, error) {
	owner, err := p.owner()
	if err != nil {
		return nil, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, err
	}
	sel := instanceSelector(owner, c.Record.ID)
	match, err := labels.Parse(sel)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(p.ctx)
	w := &watcher{pool: p, cluster: c, ns: c.Namespace(), match: match, cancel: cancel, done: make(chan struct{}), synced: make(chan struct{})}
	pods := cs.CoreV1().Pods(w.ns)
	sets := cs.AppsV1().StatefulSets(w.ns)
	w.pods = w.informer(cs, &corev1.Pod{}, "instance pods", sel,
		func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return pods.List(ctx, o) },
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return pods.Watch(ctx, o) })
	w.sets = w.informer(cs, &appsv1.StatefulSet{}, "instance StatefulSets", sel,
		func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return sets.List(ctx, o) },
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return sets.Watch(ctx, o) })
	if _, err := w.pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.pod(obj, false) },
		UpdateFunc: func(_, obj any) { w.pod(obj, false) },
		DeleteFunc: func(obj any) { w.pod(obj, true) },
	}); err != nil {
		cancel()
		return nil, err
	}
	if _, err := w.sets.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.set(obj, false) },
		UpdateFunc: func(_, obj any) { w.set(obj, false) },
		DeleteFunc: func(obj any) { w.set(obj, true) },
	}); err != nil {
		cancel()
		return nil, err
	}
	var wg sync.WaitGroup
	for _, inf := range []cache.SharedIndexInformer{w.pods, w.sets} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inf.RunWithContext(ctx)
		}()
	}
	go func() {
		wg.Wait()
		close(w.done)
	}()
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), w.pods.HasSynced, w.sets.HasSynced) {
			close(w.synced)
			p.watchSynced(w)
		}
	}()
	return w, nil
}

// informer is one kind's informer over list and watch, both narrowed to sel,
// recording whether the API server answers.
func (w *watcher) informer(cs kclient.Interface, example runtime.Object, what, sel string,
	list func(context.Context, metav1.ListOptions) (runtime.Object, error),
	watchFn func(context.Context, metav1.ListOptions) (watch.Interface, error)) cache.SharedIndexInformer {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.LabelSelector = sel
			obj, err := list(ctx, o)
			w.answered(ctx, err)
			return obj, err
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = sel
			wi, err := watchFn(ctx, o)
			w.answered(ctx, err)
			return wi, err
		},
	}
	inf := cache.NewSharedIndexInformerWithOptions(cache.ToListWatcherWithWatchListSemantics(lw, cs), example,
		cache.SharedIndexInformerOptions{ObjectDescription: what})
	// Watch failures go to fleet.log, not to fleetd's stderr through klog,
	// and mark the connection unreachable until a list or watch works again.
	_ = inf.SetWatchErrorHandlerWithContext(func(ctx context.Context, _ *cache.Reflector, err error) {
		switch {
		case err == nil, errors.Is(err, io.EOF), apierrors.IsResourceExpired(err), apierrors.IsGone(err):
			// A watch that closed normally or ran past its history: the
			// reflector lists again.
			return
		}
		w.answered(ctx, err)
	})
	return inf
}

// answered records whether the API server answered a list or a watch.
func (w *watcher) answered(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	changed := (w.err == nil) != (err == nil)
	if err != nil && w.err == nil {
		w.pool.opts.Logger.Warn("kube watch: the API server does not answer", "cluster", w.cluster.Record.ID, "err", err)
	}
	if err == nil && w.err != nil {
		w.pool.opts.Logger.Info("kube watch: the API server answers again", "cluster", w.cluster.Record.ID)
	}
	w.err = err
	w.mu.Unlock()
	if changed {
		w.pool.watchHealthChanged(w)
	}
}

// health is why the informers' view may be stale: the API server not
// answering (or refusing the list: a login that expired, missing RBAC), or
// not synced yet (nil when it is current).
func (w *watcher) health() error {
	w.mu.Lock()
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-w.synced:
		return nil
	default:
		return errNotSynced
	}
}

var errNotSynced = errors.New("the cluster's instances are not listed yet")

func (w *watcher) stop() {
	w.cancel()
}

// pod hands a pod event to the tracker.
func (w *watcher) pod(obj any, deleted bool) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj, deleted = tomb.Obj, true
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok || !w.match.Matches(labels.Set(pod.Labels)) {
		return
	}
	sts, ok := strings.CutSuffix(pod.Name, "-0")
	if !ok || sts == "" {
		return
	}
	w.pool.podEvent(w, sts, pod, deleted)
}

// set hands a StatefulSet event to the tracker.
func (w *watcher) set(obj any, deleted bool) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj, deleted = tomb.Obj, true
	}
	set, ok := obj.(*appsv1.StatefulSet)
	if !ok || !w.match.Matches(labels.Set(set.Labels)) {
		return
	}
	w.pool.setEvent(w, set, deleted)
}

// cachedPod and cachedSet read the informers' copies (nil when absent).
func (w *watcher) cachedPod(name string) *corev1.Pod {
	obj, ok, err := w.pods.GetIndexer().GetByKey(w.ns + "/" + name)
	if err != nil || !ok {
		return nil
	}
	pod, _ := obj.(*corev1.Pod)
	return pod
}

func (w *watcher) cachedSet(name string) *appsv1.StatefulSet {
	obj, ok, err := w.sets.GetIndexer().GetByKey(w.ns + "/" + name)
	if err != nil || !ok {
		return nil
	}
	set, _ := obj.(*appsv1.StatefulSet)
	return set
}

// cacheObjects reads objects from a synced watcher's informers, falling back
// to the API server for what they do not hold (an object made a moment ago,
// one outside the instance labels) or while they are not current.
type cacheObjects struct {
	w   *watcher
	api Objects
}

func (o cacheObjects) Pod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	if ns == o.w.ns && o.w.health() == nil {
		if pod := o.w.cachedPod(name); pod != nil {
			return pod.DeepCopy(), nil
		}
	}
	return o.api.Pod(ctx, ns, name)
}

func (o cacheObjects) StatefulSet(ctx context.Context, ns, name string) (*appsv1.StatefulSet, error) {
	if ns == o.w.ns && o.w.health() == nil {
		if set := o.w.cachedSet(name); set != nil {
			return set.DeepCopy(), nil
		}
	}
	return o.api.StatefulSet(ctx, ns, name)
}

// watcherFor is c's running watcher, started on first use. A connection
// whose client was reloaded gets fresh informers (the old ones stop); the
// trackers keep what they learned.
func (p *Pool) watcherFor(c *Cluster) (*watcher, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("the Kubernetes pool is closed")
	}
	w := p.watchers[c.Record.ID]
	if w != nil && w.cluster == c {
		p.mu.Unlock()
		return w, nil
	}
	reloaded := w != nil
	if w != nil {
		delete(p.watchers, c.Record.ID)
		w.stop()
	}
	p.mu.Unlock()
	if reloaded {
		// The node watches run on the old client too.
		p.nodeMu.Lock()
		p.stopNodeWatchesLocked(c.Record.ID)
		p.nodeMu.Unlock()
		defer p.syncNodeWatches(c.Record.ID)
	}
	nw, err := p.startWatcher(c)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur := p.watchers[c.Record.ID]; cur != nil || p.closed {
		// Another caller started one meanwhile, or the pool closed.
		nw.stop()
		if cur == nil {
			return nil, errors.New("the Kubernetes pool is closed")
		}
		return cur, nil
	}
	p.watchers[c.Record.ID] = nw
	return nw, nil
}

// runningWatcher is the connection's watcher if one runs (it never starts
// one).
func (p *Pool) runningWatcher(clusterID string) *watcher {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watchers[clusterID]
}

// stopWatcherLocked stops a removed connection's informers.
func (p *Pool) stopWatcherLocked(clusterID string) {
	if w := p.watchers[clusterID]; w != nil {
		delete(p.watchers, clusterID)
		w.stop()
	}
}
