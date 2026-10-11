//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// pfpool.go is the port-forward pool: fleetd's TCP connections into its
// instance pods (the Forward data plane, the browser proxy, published ports).
// Each verified pod gets one pods/portforward connection, kept per pod UID and
// shared by every TCP connection to it, each one stream pair on it, so a page
// load with dozens of connections costs one API server upgrade. A connection
// that ends (an API server rollout, a ping that failed) is dropped and the
// next dial opens a new one; one that carried nothing for PortForwardIdle is
// closed. A pod is verified before every dial (from the informers' cache
// while they run), so a substituted pod never gets a byte, and a replaced pod
// (another UID) gets a connection of its own. A connection the cluster forbids
// (no pods/portforward permission) is remembered for a while, so the caller's
// fallback (the pod agent's forward stream) is taken at once.

// Port-forward pool defaults.
const (
	// DefaultPortForwardIdle is how long an idle port-forward connection
	// stays open.
	DefaultPortForwardIdle = 5 * time.Minute
	// portForwardForbiddenRecheck is how long a connection's port-forward
	// refusal is believed before it is asked again (a permission granted
	// meanwhile is picked up then).
	portForwardForbiddenRecheck = 5 * time.Minute
)

// portForwards is the pool's port-forward state.
type portForwards struct {
	mu        sync.Mutex
	pods      map[podKey]*pfEntry
	forbidden map[string]time.Time // connection -> refused until
	reaping   bool
	closed    bool
}

// pfEntry is one pod's connection, or the dial of it in flight.
type pfEntry struct {
	uid    types.UID
	pf     *kubeexec.PortForward
	flight chan struct{}
	err    error
}

func (p *Pool) pfState() *portForwards {
	p.pfOnce.Do(func() {
		p.pf = &portForwards{pods: map[podKey]*pfEntry{}, forbidden: map[string]time.Time{}}
	})
	return p.pf
}

// DialPort connects to port on the loopback of t's pod: one stream pair on
// the pod's pooled port-forward connection. The pod is verified first. The
// connection's CloseWrite ends the stream's write side, which the cluster's
// runtime does not pass on to the pod's peer (containerd ends the connection
// a second later instead); a port nothing listens on shows as the first Read
// failing (*kubeexec.PortError). An error wrapping
// kubeexec.ErrPortForwardForbidden is the cluster refusing port-forwards.
func (p *Pool) DialPort(ctx context.Context, t PodTarget, port int) (io.ReadWriteCloser, error) {
	pf, err := p.portForward(ctx, t)
	if err != nil {
		return nil, err
	}
	conn, err := pf.Dial(port)
	if errors.Is(err, kubeexec.ErrPortForwardClosed) {
		// It ended between the lookup and the dial: once more on a new one.
		if pf, err = p.portForward(ctx, t); err != nil {
			return nil, err
		}
		conn, err = pf.Dial(port)
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// WarmPorts opens t's pod's port-forward connection ahead of the first dial
// (a browser open, whose first page load would otherwise wait for it).
func (p *Pool) WarmPorts(ctx context.Context, t PodTarget) error {
	_, err := p.portForward(ctx, t)
	return err
}

// portForward is the verified pod's live port-forward connection, opened
// when there is none (concurrent callers share one dial).
func (p *Pool) portForward(ctx context.Context, t PodTarget) (*kubeexec.PortForward, error) {
	c, err := p.Cluster(t.Cluster)
	if err != nil {
		return nil, err
	}
	pod, err := p.verify(ctx, c, t)
	if err != nil {
		return nil, err
	}
	s := p.pfState()
	key := podKey{c.Record.ID, pod.Namespace, pod.Name}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errors.New("the Kubernetes pool is closed")
		}
		if until, ok := s.forbidden[c.Record.ID]; ok {
			if p.opts.Now().Before(until) {
				s.mu.Unlock()
				return nil, fmt.Errorf("%w (connection %s)", kubeexec.ErrPortForwardForbidden, c.Record.ID)
			}
			delete(s.forbidden, c.Record.ID)
		}
		e := s.pods[key]
		switch {
		case e != nil && e.uid != pod.UID:
			// The pod was replaced: its connection goes with it.
			delete(s.pods, key)
			if e.pf != nil {
				_ = e.pf.Close()
			}
			e = nil
		case e != nil && e.pf != nil && !e.pf.Alive():
			delete(s.pods, key)
			e = nil
		}
		if e == nil {
			e = &pfEntry{uid: pod.UID, flight: make(chan struct{})}
			s.pods[key] = e
			if !s.reaping {
				s.reaping = true
				go p.reapPortForwards()
			}
			s.mu.Unlock()
			go p.dialPortForward(c, key, e)
		} else {
			s.mu.Unlock()
		}
		select {
		case <-e.flight:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err != nil {
			return nil, e.err
		}
		if e.pf.Alive() {
			return e.pf, nil
		}
		// It ended already: the next round opens another.
	}
}

// dialPortForward opens e's connection, bounded by the handshake deadline
// rather than one caller's context (its other callers share it).
func (p *Pool) dialPortForward(c *Cluster, key podKey, e *pfEntry) {
	ctx, cancel := context.WithTimeout(p.ctx, p.opts.HandshakeTimeout)
	defer cancel()
	var pf *kubeexec.PortForward
	var err error
	if p.opts.PortForward != nil {
		pf, err = p.opts.PortForward(ctx, c, key.namespace, key.pod)
	} else {
		pf, err = c.Conn.PortForward(ctx, key.namespace, key.pod)
	}
	s := p.pfState()
	s.mu.Lock()
	switch {
	case err != nil:
		if s.pods[key] == e {
			delete(s.pods, key)
		}
		if errors.Is(err, kubeexec.ErrPortForwardForbidden) {
			if _, known := s.forbidden[c.Record.ID]; !known {
				p.opts.Logger.Warn("kube: the cluster forbids pods/portforward; forwards to its pods go through the pod agent", "cluster", c.Record.ID, "err", err)
			}
			s.forbidden[c.Record.ID] = p.opts.Now().Add(portForwardForbiddenRecheck)
		}
		e.err = classify(err)
	case s.closed || s.pods[key] != e:
		// The pool closed, or the pod was forgotten, meanwhile.
		_ = pf.Close()
		e.err = errors.New("the pod's port-forward was dropped")
	default:
		e.pf = pf
		go p.dropWhenDone(key, e)
	}
	s.mu.Unlock()
	close(e.flight)
}

// dropWhenDone forgets e's connection once it ends.
func (p *Pool) dropWhenDone(key podKey, e *pfEntry) {
	<-e.pf.Done()
	s := p.pfState()
	s.mu.Lock()
	if s.pods[key] == e {
		delete(s.pods, key)
	}
	s.mu.Unlock()
}

// reapPortForwards closes connections idle for PortForwardIdle, until the
// pool closes.
func (p *Pool) reapPortForwards() {
	idle := p.opts.PortForwardIdle
	t := time.NewTicker(max(idle/5, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			p.ClosePortForwards()
			return
		case <-t.C:
		}
		s := p.pfState()
		var idleOnes []*kubeexec.PortForward
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		for key, e := range s.pods {
			if e.pf == nil {
				continue
			}
			if since := e.pf.IdleSince(); e.pf.Active() == 0 && !since.IsZero() && time.Since(since) >= idle {
				delete(s.pods, key)
				idleOnes = append(idleOnes, e.pf)
			}
		}
		s.mu.Unlock()
		for _, pf := range idleOnes {
			_ = pf.Close()
		}
	}
}

// forgetPortForward closes the pod's port-forward connection (it was
// destroyed or rebuilt).
func (p *Pool) forgetPortForward(clusterID, pod string) {
	s := p.pfState()
	var gone []*kubeexec.PortForward
	s.mu.Lock()
	for key, e := range s.pods {
		if key.cluster == clusterID && key.pod == pod {
			delete(s.pods, key)
			if e.pf != nil {
				gone = append(gone, e.pf)
			}
		}
	}
	s.mu.Unlock()
	for _, pf := range gone {
		_ = pf.Close()
	}
}

// PortForwards is how many port-forward connections are open.
func (p *Pool) PortForwards() int {
	s := p.pfState()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.pods {
		if e.pf != nil && e.pf.Alive() {
			n++
		}
	}
	return n
}

// ClosePortForwards closes every port-forward connection, and the TCP
// connections on them, for good: the bounded shutdown's port-forward step.
// Later dials fail.
func (p *Pool) ClosePortForwards() {
	s := p.pfState()
	s.mu.Lock()
	s.closed = true
	var all []*kubeexec.PortForward
	for key, e := range s.pods {
		delete(s.pods, key)
		if e.pf != nil {
			all = append(all, e.pf)
		}
	}
	s.mu.Unlock()
	for _, pf := range all {
		_ = pf.Close()
	}
}
