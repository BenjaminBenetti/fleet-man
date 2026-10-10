//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// PodTarget is an instance pod and what fleetd recorded about it.
type PodTarget struct {
	// Cluster is the connection (KubeCluster.ID).
	Cluster string
	// Fleet and Instance tag the pod's log lines.
	Fleet, Instance string
	// StatefulSet is the instance's StatefulSet; its pod is
	// names.Pod(StatefulSet).
	StatefulSet string
	// Expect is what the pod must match before it gets anything.
	Expect Expect
	// SpecSecret is the current generation's spec Secret
	// (names.SpecSecret): a restage writes the new binary's sha into its
	// pid1.json. Empty skips that (a pod without one).
	SpecSecret string
	// Priority orders this bootstrap among the connection's waiting ones.
	Priority Priority
	// Restage stages the daemon's build when the pod runs another one: the
	// lazy moments (a start, a mic or output open, an idle pod). Without
	// it, an agent of another build that speaks a common protocol is used
	// as it is.
	Restage bool
}

// Pod is the target's pod name.
func (t PodTarget) Pod() string { return names.Pod(t.StatefulSet) }

type podKey struct{ cluster, namespace, pod string }

// podSlot is the pool's state for one pod: its session, the bootstrap in
// flight, the backoff after failures, the fast path, and the log limiter and
// audio activity every session of the pod shares.
type podSlot struct {
	pool     *Pool
	key      podKey
	logs     *podwire.LogLimiter
	activity *podwire.AudioActivity

	mu       sync.Mutex
	session  *AgentSession
	flight   *bootFlight
	failures int
	retryAt  time.Time
	fast     *fastPath
	gone     bool
	// held is every resumable command started on the pod's sessions
	// (AgentSession.Exec), re-attached to the next session after a drop.
	held map[*podwire.Exec]*heldExec
	// flowingAtDrop: audio was flowing on the pod when its last session
	// dropped, so its reconnect is interactive.
	flowingAtDrop bool
}

type bootFlight struct {
	done    chan struct{}
	restage bool
	// ticket is the bootstrap's place in the connection's limiter, raised
	// to the priority of every caller that joins it.
	ticket  *ticket
	session *AgentSession
	err     error
}

// fastPath is what an earlier bootstrap learned about a pod: for the same
// pod (UID), whose image cannot have changed, the probe is skipped.
type fastPath struct {
	uid   string
	probe jobboot.Probe
	build *podBuild
}

func (p *Pool) slot(c *Cluster, t PodTarget) (*podSlot, error) {
	key := podKey{c.Record.ID, c.Namespace(), t.Pod()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("the Kubernetes pool is closed")
	}
	s, ok := p.slots[key]
	if !ok {
		logger := p.opts.Logger.With("fleet", t.Fleet, "instance", t.Instance)
		s = &podSlot{pool: p, key: key, logs: podwire.NewLogLimiter(logger, 0, 0), activity: &podwire.AudioActivity{}}
		p.slots[key] = s
	}
	return s, nil
}

// EnsureAgent returns the pod's agent session, bootstrapping one when none
// is up (or t.Restage asks for the daemon's build and the session runs
// another): verify, probe (skipped on the fast path), stage when needed,
// then the agent exec and its handshake. Concurrent calls for one pod share
// one bootstrap, which runs at the highest priority among them; ctx bounds
// only this caller's wait. A pod whose bootstrap failed waits a full-jitter
// backoff before the next one, and every bootstrap waits for one of its
// connection's slots. A lazy restage never costs a pod its working session:
// when it fails, or a recent failure's backoff is still running, the
// session of the older build is returned. Only a pod that no longer
// verifies loses its session (kubernetes:tampered).
func (p *Pool) EnsureAgent(ctx context.Context, t PodTarget) (*AgentSession, error) {
	if t.StatefulSet == "" {
		return nil, errors.New("no StatefulSet named for the pod")
	}
	c, err := p.Cluster(t.Cluster)
	if err != nil {
		return nil, err
	}
	slot, err := p.slot(c, t)
	if err != nil {
		return nil, err
	}
	if t.Restage {
		if s := slot.live(); s != nil {
			// A session started while the store had no build for the
			// pod's arch is judged against the store's build now.
			s.refreshWant(ctx)
		}
	}
	for attempt := 0; ; attempt++ {
		slot.mu.Lock()
		if s := slot.session; s != nil && s.alive() {
			// The restage of a stale session that failed a moment ago waits
			// out its backoff on the session it has.
			if !(t.Restage && s.Stale()) || (slot.flight == nil && slot.retryAt.After(p.opts.Now())) {
				slot.mu.Unlock()
				return s, nil
			}
		}
		f := slot.flight
		if f == nil {
			f = &bootFlight{done: make(chan struct{}), restage: t.Restage, ticket: newTicket(t.Priority)}
			slot.flight = f
			go slot.boot(c, t, f)
		} else {
			f.ticket.raise(t.Priority)
		}
		slot.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if f.err != nil {
			if t.Restage {
				if s := slot.live(); s != nil {
					return s, nil
				}
			}
			return nil, f.err
		}
		// A restage joined a bootstrap that was not one: ask again, once.
		if t.Restage && !f.restage && f.session.Stale() && attempt == 0 {
			continue
		}
		return f.session, nil
	}
}

// live is the slot's session while it runs.
func (s *podSlot) live() *AgentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.session; sess != nil && sess.alive() {
		return sess
	}
	return nil
}

// Session is the pod's live agent session, if one is up; it never
// bootstraps.
func (p *Pool) Session(t PodTarget) *AgentSession {
	c, err := p.Cluster(t.Cluster)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	slot := p.slots[podKey{c.Record.ID, c.Namespace(), t.Pod()}]
	p.mu.Unlock()
	if slot == nil {
		return nil
	}
	return slot.live()
}

// Forget drops what the pool keeps for the pod: its session and the
// commands it held, its fast path and backoff, what the informers showed of
// it, and its supervision (the instance was destroyed or rebuilt). It needs
// no working connection: a pod is forgotten even while its kubeconfig does
// not load.
func (p *Pool) Forget(t PodTarget) {
	pod := t.Pod()
	var gone []*podSlot
	p.mu.Lock()
	for key, slot := range p.slots {
		if key.cluster == t.Cluster && key.pod == pod {
			gone = append(gone, slot)
			delete(p.slots, key)
		}
	}
	p.mu.Unlock()
	k := trackKey{t.Cluster, t.StatefulSet}
	p.instMu.Lock()
	delete(p.tracks, k)
	delete(p.supervised, k)
	delete(p.watched, k)
	p.instMu.Unlock()
	for _, slot := range gone {
		slot.forget()
	}
}

// Verify reads the target's StatefulSet and pod and verifies the pod
// (VerifyPod).
func (p *Pool) Verify(ctx context.Context, t PodTarget) (*corev1.Pod, error) {
	c, err := p.Cluster(t.Cluster)
	if err != nil {
		return nil, err
	}
	return p.verify(ctx, c, t)
}

func (p *Pool) verify(ctx context.Context, c *Cluster, t PodTarget) (*corev1.Pod, error) {
	objs, err := p.objects(c)
	if err != nil {
		return nil, err
	}
	ns := c.Namespace()
	sts, err := objs.StatefulSet(ctx, ns, t.StatefulSet)
	if err != nil {
		return nil, fmt.Errorf("instance StatefulSet %s/%s: %w", ns, t.StatefulSet, err)
	}
	pod, err := objs.Pod(ctx, ns, t.Pod())
	if err != nil {
		return nil, fmt.Errorf("instance pod %s/%s: %w", ns, t.Pod(), err)
	}
	if err := VerifyPod(pod, sts, t.Expect, p.opts.SpecHash); err != nil {
		return nil, err
	}
	return pod, nil
}

// boot runs one bootstrap for the slot and publishes its result.
func (s *podSlot) boot(c *Cluster, t PodTarget, f *bootFlight) {
	sess, err := s.bootstrap(c, t, f.ticket)
	p := s.pool
	var tampered *TamperedError
	s.mu.Lock()
	var replaced *AgentSession
	switch {
	case err == nil && s.gone:
		err = ErrSessionClosed
	case err == nil:
		s.failures, s.retryAt = 0, time.Time{}
		s.flowingAtDrop = false
		replaced, s.session = s.session, sess
	case errors.Is(err, ErrPodNotReady):
		// Not a failure of the pod's: it is starting (or going), and the
		// informer's Ready event, not a backoff, says when to try again.
	default:
		s.failures++
		s.retryAt = p.opts.Now().Add(fullJitter(s.failures, p.opts.BackoffBase, p.opts.BackoffCap, p.opts.Rand()))
		switch old := s.session; {
		case old == nil || !old.alive():
		case errors.As(err, &tampered):
			// The pod a session runs in no longer verifies (an
			// ephemeral container was added, say): it gets nothing more.
			replaced, s.session = old, nil
		case f.restage:
			p.opts.Logger.Warn("kube agent: restage failed; keeping the pod's older agent", "fleet", t.Fleet, "instance", t.Instance, "agent_version", versionOrDev(old.Hello().Version), "err", err)
		}
	}
	f.session, f.err = sess, err
	s.flight = nil
	s.mu.Unlock()
	if err != nil && sess != nil {
		_ = sess.Close()
	}
	if replaced != nil && replaced != sess {
		// The new agent took the pod's agent role over from the old one,
		// held commands included; close what is left of the old session.
		_ = replaced.Close()
	}
	close(f.done)
	if sts, ok := cutPodSuffix(s.key.pod); ok {
		k := trackKey{s.key.cluster, sts}
		switch {
		case err == nil:
			p.noteTampered(k, nil)
		case errors.As(err, &tampered):
			p.noteTampered(k, tampered)
		}
	}
	if err == nil {
		go s.watch(sess)
		go s.reattach(sess)
		if sts, ok := cutPodSuffix(s.key.pod); ok {
			p.changed(trackKey{s.key.cluster, sts})
		}
	}
}

// watch drops the session from the slot once it ends.
func (s *podSlot) watch(sess *AgentSession) {
	<-sess.Done()
	sess.mu.Lock()
	b := sess.bulk
	sess.bulk = nil
	sess.mu.Unlock()
	if b != nil {
		_ = b.Close()
	}
	_ = sess.stream.Close()
	s.mu.Lock()
	current := s.session == sess
	if current {
		s.session = nil
		s.flowingAtDrop = s.activity.Flowing()
	}
	gone := s.gone
	s.mu.Unlock()
	if err := sess.Err(); err != nil && !errors.Is(err, podwire.ErrClosed) {
		s.pool.opts.Logger.Info("kube agent: session ended", "cluster", s.key.cluster, "pod", s.key.namespace+"/"+s.key.pod, "err", err)
	}
	if current && !gone {
		s.detachHeld()
		s.pool.sessionEnded(s)
	}
}

// bootstrap is one attempt: backoff, a connection slot (at the ticket's
// priority), verification, the probe (or the fast path), a stage when
// needed, and the agent.
func (s *podSlot) bootstrap(c *Cluster, t PodTarget, tk *ticket) (*AgentSession, error) {
	p := s.pool
	s.mu.Lock()
	wait := s.retryAt.Sub(p.opts.Now())
	s.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-p.ctx.Done():
			timer.Stop()
			return nil, p.ctx.Err()
		}
	}
	release, err := c.gates.boot.acquireTicket(p.ctx, tk)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(p.ctx, p.opts.HandshakeTimeout)
	defer cancel()
	start := p.opts.Now()

	pod, err := p.verify(ctx, c, t)
	if err != nil {
		var tampered *TamperedError
		if errors.As(err, &tampered) {
			p.opts.Logger.Error("kube agent: refused a pod that is not the one fleet made", "fleet", t.Fleet, "instance", t.Instance, "err", err)
		}
		return nil, err
	}
	ref := PodRef{Cluster: c.Record.ID, Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}

	s.mu.Lock()
	fast := s.fast
	s.mu.Unlock()
	probed := false
	if fast == nil || fast.uid != string(pod.UID) {
		probe, build, err := p.probe(ctx, c, pod.Name)
		if err != nil {
			return nil, err
		}
		fast, probed = &fastPath{uid: string(pod.UID), probe: probe, build: build}, true
	}

	art, artErr := p.bins().Binary(ctx, fast.probe.Arch)
	staged := ""
	if reason := stageReason(fast.build, art, t.Restage); reason != "" {
		if artErr != nil {
			return nil, fmt.Errorf("%s, and fleetd has no build to stage: %w", reason, artErr)
		}
		prev, err := p.stageBinary(ctx, c, pod.Name, fast.probe, art)
		if err != nil {
			s.dropFast()
			return nil, err
		}
		staged = art.SHA256
		if t.SpecSecret != "" {
			cs, err := p.clientset(c)
			if err == nil {
				err = patchPID1Binary(ctx, cs, ref.Namespace, t.SpecSecret, art.SHA256, prev)
			}
			if err != nil {
				p.opts.Logger.Warn("kube agent: pod-init would fall back to the old fleet binary at the next start", "fleet", t.Fleet, "instance", t.Instance, "secret", t.SpecSecret, "err", err)
			}
		}
	} else if artErr != nil && fast.build != nil {
		p.opts.Logger.Warn("kube agent: no fleet build of the daemon's version to stage; using the pod's", "fleet", t.Fleet, "instance", t.Instance, "pod_version", fast.build.Version, "err", artErr)
	}

	client, stream, err := p.dialAgent(ctx, c, s, ref, podwire.RoleMain, nil)
	if err != nil {
		s.dropFast()
		return nil, err
	}
	h := client.Hello()
	s.mu.Lock()
	s.fast = &fastPath{uid: string(pod.UID), probe: fast.probe, build: &podBuild{Version: h.Version, SHA: h.SHA, ProtoMin: h.ProtoMin, ProtoMax: h.ProtoMax}}
	s.mu.Unlock()
	sess := &AgentSession{pool: p, slot: s, cluster: c, pod: ref, probe: fast.probe, main: client, stream: stream, pacer: podwire.NewPacer(s.activity.Flowing)}
	if art != nil {
		sess.setWant(art.SHA256)
	}
	p.opts.Logger.Info("kube agent: connected", "fleet", t.Fleet, "instance", t.Instance, "pod", ref.Namespace+"/"+ref.Name,
		"probed", probed, "staged", staged != "", "agent_version", versionOrDev(h.Version), "stale", sess.Stale(), "ms", p.opts.Now().Sub(start).Milliseconds())
	return sess, nil
}

func (s *podSlot) dropFast() {
	s.mu.Lock()
	s.fast = nil
	s.mu.Unlock()
}

// probe is exec #1.
func (p *Pool) probe(ctx context.Context, c *Cluster, pod string) (jobboot.Probe, *podBuild, error) {
	var out, errOut bytes.Buffer
	req := kubeexec.ExecRequest{Pod: pod, Container: DevContainer, Command: []string{"sh", "-c", probeScript(p.opts.Layout)}}
	if err := p.exec(ctx, c, req, kubeexec.Stdio{Stdout: &limitedWriter{w: &out, n: 64 << 10}, Stderr: &limitedWriter{w: &errOut, n: 4 << 10}}); err != nil {
		return jobboot.Probe{}, nil, fmt.Errorf("probing pod %s: %w%s", pod, err, detail(errOut.String()))
	}
	probe, build, err := parseProbe(out.String())
	if err != nil {
		return probe, nil, fmt.Errorf("probing pod %s: %w", pod, err)
	}
	return probe, build, nil
}

// stageReason says why the pod needs the daemon's build staged ("" when it
// does not): it has no working fleet, or one that speaks no protocol fleetd
// does, or (restage) another build than art.
func stageReason(build *podBuild, art *binstore.Artifact, restage bool) string {
	switch {
	case build == nil:
		return "the pod has no working fleet binary"
	case !protoOverlaps(build):
		return fmt.Sprintf("the pod's fleet %s speaks protocol %d-%d, this fleetd %d-%d", versionOrDev(build.Version), build.ProtoMin, build.ProtoMax, podwire.ProtoMin, podwire.ProtoMax)
	case art == nil || build.SHA == art.SHA256:
		return ""
	case restage:
		return "the pod runs another fleet build"
	}
	return ""
}

func protoOverlaps(b *podBuild) bool {
	_, err := podwire.Negotiate(podwire.ProtoMin, podwire.ProtoMax, b.ProtoMin, b.ProtoMax)
	return err == nil
}

func versionOrDev(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// forget closes the slot's session for good, with the commands it held.
func (s *podSlot) forget() {
	s.mu.Lock()
	s.gone = true
	sess := s.session
	s.session = nil
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	s.failHeld(func(types.UID) bool { return true })
	s.logs.Close()
}

// shutdown is forget, telling the agent first and waiting within ctx.
func (s *podSlot) shutdown(ctx context.Context) {
	s.mu.Lock()
	s.gone = true
	sess := s.session
	s.session = nil
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Shutdown(ctx)
	}
	s.logs.Close()
}
