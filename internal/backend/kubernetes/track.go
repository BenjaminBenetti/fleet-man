//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// track.go is what the informers show of each instance over time: the
// restarts the cluster made on its own (RestartAware's source), crash loops,
// and the runs of the dev container that ended without a Fleet stop. It runs
// on informer events in fleetd, whether or not a TUI is connected.
//
// A restart is a change of the instance pod's UID (the pod was evicted,
// drained, lost with its node, or deleted, and the StatefulSet made a new
// one) or of its dev container's restart count (the container was OOM-killed
// or crashed, and the kubelet started it again). A pod that goes while the
// StatefulSet is scaled to 0 (a Fleet stop) and the one a start brings are not
// restarts, nor is a replacement fleet announced (ExpectReplacement). The
// first pod seen of an instance is its baseline: a restart made while fleetd
// was not watching is not reported.

// Crash-loop rule: this many restarts of the dev container (crashes and
// out-of-memory kills, not pods the cluster replaced) within
// crashLoopWindow, or the kubelet backing the dev container off
// (CrashLoopBackOff), park the instance.
const (
	crashLoopRestarts = 3
	crashLoopWindow   = 5 * time.Minute
	// expectationTTL bounds how long an announced replacement waits.
	expectationTTL = 10 * time.Minute
)

// Error codes of a parked instance.
const (
	// CodeOOMKilled: the dev container ran out of memory again and again.
	CodeOOMKilled = "kubernetes:oomkilled"
	// CodeCrashLoop: the dev container kept crashing.
	CodeCrashLoop = "kubernetes:crash_loop"
)

// CrashLoop is an instance the cluster keeps restarting.
type CrashLoop struct {
	// OOM: the last run ended in an out-of-memory kill.
	OOM bool
	// Restarts is how many restarts fell within the window (0 when the
	// kubelet's back-off said so first).
	Restarts int
	// BackOff: the kubelet is backing the container off (CrashLoopBackOff).
	BackOff bool
	// ExitCode and Reason are the last run's ("OOMKilled", "Error").
	ExitCode int32
	Reason   string
	// MemoryLimit is the dev container's memory limit ("" when it has none).
	MemoryLimit string
}

// Code is the parked instance's error code.
func (l CrashLoop) Code() string {
	if l.OOM {
		return CodeOOMKilled
	}
	return CodeCrashLoop
}

// Message says what happened, for the instance's warning.
func (l CrashLoop) Message() string {
	what := "kept crashing"
	if l.OOM {
		what = "kept running out of memory"
		if l.MemoryLimit != "" {
			what += " (memory limit " + l.MemoryLimit + ")"
		}
	} else if l.ExitCode != 0 {
		what += fmt.Sprintf(" (exit code %d)", l.ExitCode)
	}
	when := "and the cluster is backing it off"
	if l.Restarts > 0 {
		when = fmt.Sprintf("%d restarts within %s", l.Restarts, crashLoopWindow)
	}
	hint := "`fleet logs` shows its runs; start it again once fixed"
	if l.OOM {
		hint = "raise memory_limit in the cluster's resources, then start it again"
	}
	return fmt.Sprintf("%s: the dev container %s, %s. Parked as stopped: %s.", l.Code(), what, when, hint)
}

// RestartHint is the connection setting that keeps a restart of cause from
// happening again, in the words the instance row shows ("" when no setting
// does: a crash, a lost node, a deleted pod).
func RestartHint(cause backend.RestartCause) string {
	switch cause {
	case backend.RestartOOMKilled:
		return "raise memory_limit in the cluster's resources"
	case backend.RestartEvicted:
		// Pods over their memory request are evicted first.
		return "raise memory_request in the cluster's resources"
	case backend.RestartNodeDrained:
		return "turn on protect_from_drain in the cluster's settings"
	}
	return ""
}

// RunEndReason is why a run of an instance's dev container ended without a
// Fleet stop.
type RunEndReason string

const (
	// RunRestarted: the container ended and the kubelet started it again.
	RunRestarted RunEndReason = "restarted"
	// RunTerminating: the pod is being deleted (evicted, drained, deleted).
	RunTerminating RunEndReason = "terminating"
	// RunParked: the instance is about to be parked after a crash loop.
	RunParked RunEndReason = "parked"
)

// RunEnd names a run whose output should be saved (container.log) before the
// cluster drops it: the previous container's after a restart, a terminating
// pod's, a parked instance's.
type RunEnd struct {
	Cluster, Namespace, StatefulSet, Pod string
	PodUID                               types.UID
	// RestartCount is the run's: the dev container's restart count while it
	// ran (its previous container's log is the kubelet's "previous").
	RestartCount int32
	Why          RunEndReason
}

// trackKey names an instance on a connection.
type trackKey struct{ cluster, sts string }

// track is one instance as the informers have shown it.
type track struct {
	ns  string
	set *appsv1.StatefulSet
	// setGone: the StatefulSet was deleted (seen by a watcher).
	setGone bool
	pod     *corev1.Pod

	// uid and count are the pod last seen and its dev container's restart
	// count ("" before the first pod).
	uid   types.UID
	count int32
	// stopped: the StatefulSet went to 0 replicas since the pod was seen,
	// so the pod's going is a stop and the next pod a start.
	stopped bool
	// gone is why the pod went while the instance ran ("" when it has not).
	gone backend.RestartCause
	// expected is a replacement fleet itself announced.
	expected *expectation

	// holderNode is the node the instance's pod holds its fleet's share on
	// in single-node mode ("" when it is no such holder, or has no pod
	// there): the node watches follow it (nodes.go).
	holderNode string

	last     *backend.Restart
	ended    bool // the last restart killed every process (an OOM kill)
	recent   []time.Time
	loop     *CrashLoop
	tampered *TamperedError
}

type expectation struct {
	cause backend.RestartCause
	until time.Time
}

func (p *Pool) trackLocked(k trackKey, ns string) *track {
	t := p.tracks[k]
	if t == nil {
		t = &track{}
		p.tracks[k] = t
	}
	if ns != "" {
		t.ns = ns
	}
	return t
}

// setEvent records a StatefulSet event.
func (p *Pool) setEvent(w *watcher, set *appsv1.StatefulSet, deleted bool) {
	k := trackKey{w.cluster.Record.ID, set.Name}
	p.instMu.Lock()
	t := p.trackLocked(k, w.ns)
	if deleted {
		t.set, t.setGone = nil, true
		t.resetLoop()
	} else {
		t.set, t.setGone = set, false
		if set.Spec.Replicas != nil && *set.Spec.Replicas == 0 {
			// A stop (Fleet's, a park, or someone's scale-down): what the
			// pod does now is not a restart, and a loop is over.
			t.stopped = true
			t.gone = ""
			t.resetLoop()
		}
	}
	p.instMu.Unlock()
	p.changed(k)
}

// podEvent records a pod event.
func (p *Pool) podEvent(w *watcher, sts string, pod *corev1.Pod, deleted bool) {
	k := trackKey{w.cluster.Record.ID, sts}
	now := p.opts.Now()
	var ends []RunEnd
	end := func(uid types.UID, count int32, why RunEndReason) {
		ends = append(ends, RunEnd{Cluster: k.cluster, Namespace: w.ns, StatefulSet: sts, Pod: pod.Name, PodUID: uid, RestartCount: count, Why: why})
	}
	p.instMu.Lock()
	t := p.trackLocked(k, w.ns)
	switch {
	case deleted:
		if pod.UID == t.uid {
			if t.pod != nil && t.pod.DeletionTimestamp == nil {
				end(pod.UID, t.count, RunTerminating)
			}
			if !t.stopped && t.gone == "" && t.wanted() {
				t.gone = goneCause(pod)
			}
			t.pod = nil
		}
	case t.uid == "":
		// The first pod seen of the instance: its baseline.
		t.uid, t.count, t.pod = pod.UID, restartCount(pod), pod
		if pod.DeletionTimestamp != nil {
			end(pod.UID, t.count, RunTerminating)
		}
	case pod.UID != t.uid:
		// A new pod: a restart unless the old one went in a stop, or fleet
		// replaced it itself.
		cause := t.gone
		if cause == "" && t.pod != nil {
			cause = goneCause(t.pod)
		}
		if t.stopped {
			cause = ""
			t.resetLoop()
		}
		if e := t.expected; e != nil {
			if now.Before(e.until) {
				cause = e.cause
			}
			t.expected = nil
		}
		t.uid, t.count, t.pod = pod.UID, restartCount(pod), pod
		t.stopped, t.gone, t.tampered = false, "", nil
		if cause != "" {
			// A replaced pod is no crash: it does not count toward a loop.
			t.restarted(cause, false, pod, now, 0)
		}
	default:
		// The same pod.
		if n := restartCount(pod); n > t.count {
			term := lastTermination(pod)
			cause, ended := backend.RestartCrashed, false
			if term != nil && term.Reason == "OOMKilled" {
				cause, ended = backend.RestartOOMKilled, true
			}
			end(pod.UID, n-1, RunRestarted)
			t.restarted(cause, ended, pod, now, int(n-t.count))
			t.count = n
		}
		if pod.DeletionTimestamp != nil && (t.pod == nil || t.pod.DeletionTimestamp == nil) {
			end(pod.UID, t.count, RunTerminating)
			if !t.stopped && t.wanted() {
				t.gone = goneCause(pod)
			}
		}
		t.pod = pod
	}
	if !deleted && t.uid == pod.UID && t.loop == nil && !t.stopped {
		if s := containerStatus(pod, DevContainer); s != nil && s.State.Waiting != nil && s.State.Waiting.Reason == "CrashLoopBackOff" {
			t.loop = loopOf(pod, 0, true)
		}
	}
	// The node the node watches follow (an old pod's late deletion leaves
	// the current one's).
	moved := false
	if node := holderNode(pod, deleted); (!deleted || pod.UID == t.uid) && node != t.holderNode {
		t.holderNode, moved = node, true
	}
	p.instMu.Unlock()
	if moved {
		p.syncNodeWatches(k.cluster)
	}
	if hook := p.runEnded(); hook != nil {
		for _, e := range ends {
			go hook(p.ctx, e)
		}
	}
	p.changed(k)
	p.podChanged(k, pod, deleted)
}

// restarted records a restart and judges whether the instance is crash
// looping: n is how many crashes of the dev container (several when they
// happened between two events) it counts toward the loop, 0 for a pod the
// cluster replaced (a drain, an eviction, a lost node, a deleted pod or
// fleet's own move), which is no crash.
func (t *track) restarted(cause backend.RestartCause, ended bool, pod *corev1.Pod, now time.Time, n int) {
	at := now
	if s := containerStatus(pod, DevContainer); s != nil && s.State.Running != nil && !s.State.Running.StartedAt.IsZero() {
		at = s.State.Running.StartedAt.Time
	}
	t.last = &backend.Restart{ID: string(pod.UID) + "/" + strconv.Itoa(int(restartCount(pod))), Cause: cause, At: at}
	t.ended = ended
	keep := t.recent[:0]
	for _, r := range t.recent {
		if now.Sub(r) < crashLoopWindow {
			keep = append(keep, r)
		}
	}
	for range min(n, crashLoopRestarts) {
		keep = append(keep, now)
	}
	t.recent = keep
	if len(t.recent) >= crashLoopRestarts && t.loop == nil {
		t.loop = loopOf(pod, len(t.recent), false)
	}
}

func (t *track) resetLoop() {
	t.recent, t.loop = nil, nil
}

// wanted reports whether the StatefulSet asks for a pod: true while it has
// not been seen (the two informers deliver in their own order), so a pod
// that goes then is judged when the replacement comes, and a scale to 0
// seen in between makes it a stop.
func (t *track) wanted() bool {
	return t.set == nil || t.set.Spec.Replicas == nil || *t.set.Spec.Replicas > 0
}

func loopOf(pod *corev1.Pod, restarts int, backOff bool) *CrashLoop {
	l := &CrashLoop{Restarts: restarts, BackOff: backOff}
	if term := lastTermination(pod); term != nil {
		l.OOM = term.Reason == "OOMKilled"
		l.ExitCode, l.Reason = term.ExitCode, term.Reason
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == DevContainer {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				l.MemoryLimit = q.String()
			}
		}
	}
	return l
}

func restartCount(pod *corev1.Pod) int32 {
	if s := containerStatus(pod, DevContainer); s != nil {
		return s.RestartCount
	}
	return 0
}

func lastTermination(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	s := containerStatus(pod, DevContainer)
	if s == nil {
		return nil
	}
	if s.LastTerminationState.Terminated != nil {
		return s.LastTerminationState.Terminated
	}
	return s.State.Terminated
}

// goneCause is why a pod that is going (or went) was taken away, from what
// the cluster wrote on it: the disruption condition the eviction API, the
// scheduler, the taint manager, the pod GC and the kubelet set, or the
// kubelet's own eviction reason. A pod deleted with none of those (kubectl
// delete pod, an operator) is "pod deleted".
func goneCause(pod *corev1.Pod) backend.RestartCause {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.DisruptionTarget || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case "EvictionByEvictionAPI":
			// kubectl drain, a node upgrade, the autoscaler's scale-down.
			return backend.RestartNodeDrained
		case "DeletionByTaintManager", "DeletionByPodGC":
			// The node went NotReady/unreachable, or was deleted.
			return backend.RestartNodeLost
		case "PreemptionByScheduler", "TerminationByKubelet":
			return backend.RestartEvicted
		}
	}
	switch pod.Status.Reason {
	case "Evicted":
		return backend.RestartEvicted
	case "NodeLost", "NodeShutdown", "Terminated", "Shutdown":
		return backend.RestartNodeLost
	}
	return backend.RestartPodDeleted
}

// ExpectReplacement announces that fleet itself is about to replace the
// instance's pod: the next new pod (within ten minutes) is reported as a
// restart with cause, or not at all when cause is "" (a restart fleet
// makes for its own reasons, such as delivering the image again).
func (p *Pool) ExpectReplacement(clusterID, sts string, cause backend.RestartCause) {
	k := trackKey{clusterID, sts}
	p.instMu.Lock()
	t := p.trackLocked(k, "")
	t.expected = &expectation{cause: cause, until: p.opts.Now().Add(expectationTTL)}
	p.instMu.Unlock()
}

// LastRestart is the latest restart the cluster made of the instance on its
// own since fleetd started watching it (RestartAware.LastRestart for the
// backend handle).
func (p *Pool) LastRestart(clusterID, sts string) (backend.Restart, bool) {
	p.instMu.Lock()
	defer p.instMu.Unlock()
	if t := p.tracks[trackKey{clusterID, sts}]; t != nil && t.last != nil {
		return *t.last, true
	}
	return backend.Restart{}, false
}

// OnRunEnded sets the hook told about every run of an instance's dev
// container that ends without a Fleet stop (RunEnd), so its output can be
// saved before the cluster drops it. Restarts and terminating pods are told
// from the informers on goroutines of their own; Park waits for the hook.
func (p *Pool) OnRunEnded(fn func(context.Context, RunEnd)) {
	p.hookMu.Lock()
	p.runEndedHook = fn
	p.hookMu.Unlock()
}

func (p *Pool) runEnded() func(context.Context, RunEnd) {
	p.hookMu.Lock()
	defer p.hookMu.Unlock()
	if p.runEndedHook != nil {
		return p.runEndedHook
	}
	return p.opts.RunEnded
}

// OnChange sets the hook told (on a goroutine of the pool's; it must not
// block) whenever something Observe reports of an instance may have changed:
// its pod or StatefulSet, its agent session, the connection's watch.
func (p *Pool) OnChange(fn func(clusterID, sts string)) {
	p.hookMu.Lock()
	p.changeHook = fn
	p.hookMu.Unlock()
}

func (p *Pool) changed(k trackKey) {
	p.hookMu.Lock()
	fn := p.changeHook
	if fn == nil {
		fn = p.opts.OnChange
	}
	p.hookMu.Unlock()
	if fn != nil {
		fn(k.cluster, k.sts)
	}
}

// watchHealthChanged tells about every instance of the watcher's connection.
func (p *Pool) watchHealthChanged(w *watcher) {
	for _, k := range p.trackedOn(w.cluster.Record.ID) {
		p.changed(k)
	}
}

// watchSynced runs once a watcher's caches are full: instances it has no
// objects of are missing, and the supervisor may start their agents.
func (p *Pool) watchSynced(w *watcher) {
	for _, k := range p.trackedOn(w.cluster.Record.ID) {
		p.changed(k)
		p.kick(k)
	}
}

// trackedOn is every instance of a connection the pool knows of.
func (p *Pool) trackedOn(clusterID string) []trackKey {
	p.instMu.Lock()
	defer p.instMu.Unlock()
	seen := map[trackKey]bool{}
	var out []trackKey
	for k := range p.tracks {
		if k.cluster == clusterID && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range p.supervised {
		if k.cluster == clusterID && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range p.watched {
		if k.cluster == clusterID && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
