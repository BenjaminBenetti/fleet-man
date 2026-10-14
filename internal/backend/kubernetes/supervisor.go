//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// supervisor.go keeps an agent session up in every running instance pod
// fleetd tracks with a target, driven by what changes (never by polling):
//
//   - a tracked set from fleetd's state (Track: every state change, and a
//     backstop pass) adds, re-prioritizes or drops instances;
//   - the informers report the pod running, replaced or changed: a running
//     pod without a session gets one, a session whose pod was replaced is
//     let go with the commands it held, and a session whose pod no longer
//     verifies (an ephemeral container added, say) is closed;
//   - a session that ends reconnects at once (the bootstrap's own backoff
//     paces a pod that keeps failing, or whose sessions keep dropping as
//     soon as they connect), at interactive priority when audio was flowing
//     on the pod, or an in-pod MCP stream was live, when it dropped;
//   - every resumable command started on a session (AgentSession.Exec) is
//     re-attached to the pod's next session, or failed with
//     podwire.ErrHeldStreamGone once the pod is replaced or after the hold
//     grace.
//
// A pod that is not running yet is left to the informer's next event, and
// one that fails verification waits until it (or its target) changes, so a
// tampered or unverifiable pod costs no exec and no repeated log line.

// Tracked is an instance the pool watches: its connection's informers run,
// Observe and LastRestart report it. With a Target the supervisor also keeps
// an agent session up in its pod while the pod runs.
type Tracked struct {
	Cluster     string
	StatefulSet string
	Target      *PodTarget
}

// TargetProvider is the Kubernetes backend handle's side of supervision:
// the instance's pod as fleetd recorded it (a PodTarget whose Expect comes
// from fleetd's own render and delivery records, never from the cluster).
// The daemon keeps an agent up in the pod of every running instance whose
// handle provides one; an error leaves the instance only watched.
type TargetProvider interface {
	PodTarget() (PodTarget, error)
}

// supervision is the supervisor's state for one instance.
type supervision struct {
	target PodTarget
	// running: a supervisor bootstrap is in flight.
	running bool
	// blocked is the pod (podIdentity) that failed verification or had
	// nothing to verify against: it is not tried again until it changes.
	blocked string
}

// Track replaces the set of instances the pool watches and supervises.
// Connections named start their informers (in the background); instances no
// longer listed are no longer supervised (their sessions end with their
// pods; Forget drops a destroyed one at once).
func (p *Pool) Track(items []Tracked) {
	clusters := map[string]bool{}
	watched := make(map[trackKey]bool, len(items))
	var kicks []trackKey
	p.instMu.Lock()
	for _, it := range items {
		if it.Cluster == "" || it.StatefulSet == "" {
			continue
		}
		k := trackKey{it.Cluster, it.StatefulSet}
		watched[k] = true
		clusters[it.Cluster] = true
		if it.Target == nil || it.Target.StatefulSet != it.StatefulSet || it.Target.Cluster != it.Cluster {
			delete(p.supervised, k)
			continue
		}
		sv := p.supervised[k]
		if sv == nil {
			sv = &supervision{}
			p.supervised[k] = sv
		}
		if !sameExpect(sv.target.Expect, it.Target.Expect) {
			// A new record of the pod (a rebuild, a re-delivered image) may
			// verify where the old one did not.
			sv.blocked = ""
		}
		if sv.running && it.Target.Priority > sv.target.Priority {
			// Join the bootstrap in flight, which raises its place in the
			// connection's queue.
			raised := *it.Target
			go func() { _, _ = p.EnsureAgent(p.ctx, raised) }()
		}
		sv.target = *it.Target
		kicks = append(kicks, k)
	}
	for k := range p.supervised {
		if !watched[k] {
			delete(p.supervised, k)
		}
	}
	p.watched = watched
	p.instMu.Unlock()
	for id := range clusters {
		go p.ensureWatching(id)
	}
	for _, k := range kicks {
		p.kick(k)
	}
}

func sameExpect(a, b Expect) bool {
	if a.SpecHash != b.SpecHash || a.StatefulSetUID != b.StatefulSetUID || a.FieldManager != b.FieldManager || a.ImageConfig != b.ImageConfig || len(a.ImageDigests) != len(b.ImageDigests) {
		return false
	}
	for i := range a.ImageDigests {
		if a.ImageDigests[i] != b.ImageDigests[i] {
			return false
		}
	}
	return true
}

// ensureWatching starts the connection's informers when they do not run
// (or run on a client that has since been reloaded).
func (p *Pool) ensureWatching(clusterID string) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		// The connection's health worker reports why it does not load;
		// Observe says unknown meanwhile.
		return
	}
	if _, err := p.watcherFor(c); err != nil && p.ctx.Err() == nil {
		p.opts.Logger.Warn("kube watch: cannot watch the connection's instances", "cluster", clusterID, "err", err)
	}
}

// kick starts a supervisor bootstrap for k when one is due: the instance is
// supervised, its pod runs, it has no live session, and nothing is in
// flight or blocked.
func (p *Pool) kick(k trackKey) {
	p.instMu.Lock()
	sv := p.supervised[k]
	t := p.tracks[k]
	if sv == nil || sv.running || t == nil || !podRunning(t.pod) || (sv.blocked != "" && sv.blocked == podIdentity(t.pod, p.opts.SpecHash)) {
		p.instMu.Unlock()
		return
	}
	target := sv.target
	sv.running = true
	p.instMu.Unlock()
	if p.Session(target) != nil {
		p.instMu.Lock()
		if sv := p.supervised[k]; sv != nil {
			sv.running = false
		}
		p.instMu.Unlock()
		return
	}
	go p.supervise(k, target)
}

// supervise is one supervisor bootstrap.
func (p *Pool) supervise(k trackKey, t PodTarget) {
	if p.droppedInteractive(t) {
		t.Priority = max(t.Priority, PriorityInteractive)
	}
	_, err := p.EnsureAgent(p.ctx, t)
	var tampered *TamperedError
	again := false
	p.instMu.Lock()
	if sv := p.supervised[k]; sv != nil {
		sv.running = false
		switch {
		case err == nil, p.ctx.Err() != nil, errors.Is(err, ErrPodNotReady):
			// Connected; or closing; or the pod is not up yet, which the
			// informer's next event is.
		case errors.As(err, &tampered), errors.Is(err, ErrUnverifiable):
			if tr := p.tracks[k]; tr != nil {
				sv.blocked = podIdentity(tr.pod, p.opts.SpecHash)
			}
		default:
			again = true
		}
	}
	p.instMu.Unlock()
	if again {
		time.AfterFunc(p.retryDelay(t), func() { p.kick(k) })
	}
}

// retryDelay is how long the supervisor waits before it tries a pod whose
// bootstrap failed again: the pod's own full-jitter backoff, or, for a
// failure before any bootstrap (the connection does not load), the backoff
// cap.
func (p *Pool) retryDelay(t PodTarget) time.Duration {
	for _, s := range p.slotsOf(trackKey{t.Cluster, t.StatefulSet}) {
		s.mu.Lock()
		failures, at := s.failures, s.retryAt
		s.mu.Unlock()
		if failures > 0 {
			return max(at.Sub(p.opts.Now()), 0)
		}
	}
	return p.opts.BackoffCap
}

// podIdentity is what a blocked pod must change for the supervisor to try it
// again: its UID and its spec.
func podIdentity(pod *corev1.Pod, hash func(*corev1.PodSpec) string) string {
	if pod == nil {
		return ""
	}
	return string(pod.UID) + "/" + hash(&pod.Spec)
}

// podRunning reports whether the pod's dev container runs (what a bootstrap
// needs; VerifyPod checks the rest).
func podRunning(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	s := containerStatus(pod, DevContainer)
	return s != nil && s.State.Running != nil
}

// podChanged is the supervisor's side of a pod event: a session whose pod was
// replaced lets its held commands go, one whose pod no longer verifies is
// closed, and a running pod without a session gets one.
func (p *Pool) podChanged(k trackKey, pod *corev1.Pod, deleted bool) {
	// What a pod that is gone held ended with it.
	p.dropReplaced(k, pod, deleted)
	p.instMu.Lock()
	sv := p.supervised[k]
	var t PodTarget
	if sv != nil {
		t = sv.target
	}
	p.instMu.Unlock()
	if sv == nil || deleted {
		return
	}
	if s := p.Session(t); s != nil && pod.UID == s.Pod().UID {
		if w := p.runningWatcher(k.cluster); w != nil {
			if set := w.cachedSet(k.sts); set != nil {
				var tampered *TamperedError
				if err := VerifyPod(pod, set, t.Expect, p.opts.SpecHash); errors.As(err, &tampered) {
					p.opts.Logger.Error("kube agent: a running pod no longer verifies; closing its session", "fleet", t.Fleet, "instance", t.Instance, "err", err)
					p.noteTampered(k, tampered)
					p.instMu.Lock()
					if sv := p.supervised[k]; sv != nil {
						sv.blocked = podIdentity(pod, p.opts.SpecHash)
					}
					p.instMu.Unlock()
					_ = s.Close()
					p.changed(k)
					return
				}
			}
		}
	}
	p.kick(k)
}

// dropReplaced fails the held commands of every pod of k other than pod (all
// of them when it was deleted): their processes ended with their pod.
func (p *Pool) dropReplaced(k trackKey, pod *corev1.Pod, deleted bool) {
	var keep types.UID
	if !deleted && pod != nil {
		keep = pod.UID
	}
	for _, slot := range p.slotsOf(k) {
		slot.failHeld(func(uid types.UID) bool { return uid != keep })
	}
}

// slotsOf is every slot of the instance's pod.
func (p *Pool) slotsOf(k trackKey) []*podSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*podSlot
	for key, s := range p.slots {
		if key.cluster == k.cluster && key.pod == k.sts+"-0" {
			out = append(out, s)
		}
	}
	return out
}

// noteTampered records (nil clears) that the instance's pod failed
// verification, for Observe.
func (p *Pool) noteTampered(k trackKey, err *TamperedError) {
	p.instMu.Lock()
	if t := p.tracks[k]; t != nil {
		t.tampered = err
	} else if err != nil {
		p.trackLocked(k, "").tampered = err
	}
	p.instMu.Unlock()
}

// droppedInteractive reports whether audio was flowing on the pod, or an
// in-pod MCP stream was live, when its last session dropped: its reconnect
// is interactive.
func (p *Pool) droppedInteractive(t PodTarget) bool {
	for _, s := range p.slotsOf(trackKey{t.Cluster, t.StatefulSet}) {
		s.mu.Lock()
		f := s.interactiveAtDrop
		s.mu.Unlock()
		if f {
			return true
		}
	}
	return false
}

// sessionEnded is the slot's session going: it reconnects if supervised.
func (p *Pool) sessionEnded(s *podSlot) {
	if sts, ok := cutPodSuffix(s.key.pod); ok {
		k := trackKey{s.key.cluster, sts}
		p.changed(k)
		p.kick(k)
	}
}

func cutPodSuffix(pod string) (string, bool) {
	if len(pod) > 2 && pod[len(pod)-2:] == "-0" {
		return pod[:len(pod)-2], true
	}
	return "", false
}

// heldExec is a resumable command the pool keeps for the pod it runs in.
type heldExec struct {
	uid types.UID
	// detachedAt is when its session dropped (zero while attached).
	detachedAt time.Time
}

// Exec starts a command on the session (podwire.Client.Exec). A resumable
// command is the pool's to keep: when the session drops it waits for the
// pod's next session and is re-attached to it there, or fails with
// podwire.ErrHeldStreamGone once the pod is replaced (its process ended with
// the pod), or after the hold grace without a session.
func (s *AgentSession) Exec(ctx context.Context, header podwire.ExecHeader) (*podwire.Exec, error) {
	if header.PodUID == "" {
		header.PodUID = string(s.pod.UID)
	}
	e, err := s.main.Exec(ctx, header)
	if err != nil {
		return nil, err
	}
	if e.Resumable() {
		s.slot.hold(e, s.pod.UID)
	}
	return e, nil
}

// hold keeps e until it ends.
func (s *podSlot) hold(e *podwire.Exec, uid types.UID) {
	s.mu.Lock()
	if s.held == nil {
		s.held = map[*podwire.Exec]*heldExec{}
	}
	s.held[e] = &heldExec{uid: uid}
	s.mu.Unlock()
	go func() {
		<-e.Done()
		s.mu.Lock()
		delete(s.held, e)
		s.mu.Unlock()
	}()
}

// failHeld fails the held commands whose pod drop says is gone.
func (s *podSlot) failHeld(drop func(types.UID) bool) {
	s.mu.Lock()
	var gone []*podwire.Exec
	for e, h := range s.held {
		if drop(h.uid) {
			gone = append(gone, e)
			delete(s.held, e)
		}
	}
	s.mu.Unlock()
	for _, e := range gone {
		e.Fail(podwire.ErrHeldStreamGone)
	}
}

// detachHeld notes the session's drop on every held command, and arms the
// hold grace after which those still waiting fail.
func (s *podSlot) detachHeld() {
	now := s.pool.opts.Now()
	s.mu.Lock()
	n := 0
	for _, h := range s.held {
		if h.detachedAt.IsZero() {
			h.detachedAt = now
		}
		n++
	}
	s.mu.Unlock()
	if n == 0 {
		return
	}
	grace := s.pool.opts.HoldGrace
	time.AfterFunc(grace, func() {
		cut := s.pool.opts.Now().Add(-grace)
		s.mu.Lock()
		var late []*podwire.Exec
		for e, h := range s.held {
			if !h.detachedAt.IsZero() && !h.detachedAt.After(cut) && e.Detached() != nil {
				late = append(late, e)
				delete(s.held, e)
			}
		}
		s.mu.Unlock()
		for _, e := range late {
			e.Fail(podwire.ErrHeldStreamGone)
		}
	})
}

// reattach moves the slot's held commands onto sess, the pod's new session:
// those of another pod fail, those of this one are re-attached (the agent
// replays what they wrote meanwhile).
func (s *podSlot) reattach(sess *AgentSession) {
	s.failHeld(func(uid types.UID) bool { return uid != sess.pod.UID })
	s.mu.Lock()
	var mine []*podwire.Exec
	for e := range s.held {
		mine = append(mine, e)
	}
	s.mu.Unlock()
	for _, e := range mine {
		if !awaitDetached(e, 2*time.Second) {
			// Ended, or still on a transport of its own.
			continue
		}
		ctx, cancel := context.WithTimeout(s.pool.ctx, s.pool.opts.HandshakeTimeout)
		err := sess.main.Reattach(ctx, e)
		cancel()
		switch {
		case err == nil:
			s.mu.Lock()
			if h := s.held[e]; h != nil {
				h.detachedAt = time.Time{}
			}
			s.mu.Unlock()
		case errors.Is(err, podwire.ErrHeldStreamGone):
			// Reattach ended it: the agent no longer holds it.
		default:
			s.pool.opts.Logger.Warn("kube agent: could not re-attach a held command; it waits for the next session", "cluster", s.key.cluster, "pod", s.key.namespace+"/"+s.key.pod, "err", err)
		}
	}
}

// awaitDetached waits up to d for e, whose session just dropped, to notice
// it lost its transport. False when it ended or kept one.
func awaitDetached(e *podwire.Exec, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		select {
		case <-e.Done():
			return false
		default:
		}
		if e.Detached() != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
