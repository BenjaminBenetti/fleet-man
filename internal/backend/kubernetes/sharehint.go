//go:build !nok8s

package kubernetes

import (
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// sharehint.go relays a multi-node share's change hints: the spoke runs in
// one of the fleet's pods, and its inotify misses writes made on other nodes,
// so each pod's agent watches the share (DesiredState.ShareHint) and opens a
// TagShareHint stream to fleetd, which hands each hint to the share's spoke
// (Pool.ServeSpoke's connection). fleetd takes nothing a pod says about
// whose share it is: the hint goes to the share fleetd's record of the pod
// names, for a folder that record has. Hints for a share whose spoke is not
// attached are dropped (the spoke's full scan finds the change).

// shareHintRate bounds the hints one pod's stream passes on per second; more
// are dropped.
const shareHintRate = 20

// shareHintQueue is how many hints wait for a spoke at most.
const shareHintQueue = 64

// shareHintOf is what a pod with share asks its agent to hint: its share's
// folders, when it is mounted from several nodes (nil otherwise).
func shareHintOf(share *ShareRecord) *podwire.ShareHint {
	if share == nil || topology.Mode(share.Mode) != topology.MultiNode || share.Claim == "" || len(share.Roots) == 0 {
		return nil
	}
	h := &podwire.ShareHint{}
	for _, r := range share.Roots {
		h.Roots = append(h.Roots, podwire.ShareHintRoot{Name: r.Name, Skip: slices.Clone(r.Skip)})
	}
	if len(h.Roots) > podwire.MaxHintRoots || h.Validate() != nil {
		return nil
	}
	return h
}

// shareHints are the attached spokes' hint queues, by share tree
// (ShareTree).
var shareHints = struct {
	mu    sync.Mutex
	sinks map[string]chan sharesync.Hint
}{sinks: map[string]chan sharesync.Hint{}}

// takeHints makes tree's hints go to the returned queue (the spoke being
// served) until the returned func is called.
func takeHints(tree string) (<-chan sharesync.Hint, func()) {
	ch := make(chan sharesync.Hint, shareHintQueue)
	shareHints.mu.Lock()
	shareHints.sinks[tree] = ch
	shareHints.mu.Unlock()
	return ch, func() {
		shareHints.mu.Lock()
		if shareHints.sinks[tree] == ch {
			delete(shareHints.sinks, tree)
		}
		shareHints.mu.Unlock()
	}
}

// passHint hands h to tree's spoke; false when none is attached, or its
// queue is full.
func passHint(tree string, h sharesync.Hint) bool {
	shareHints.mu.Lock()
	ch := shareHints.sinks[tree]
	shareHints.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- h:
		return true
	default:
		return false
	}
}

// serveShareHint serves one share-hint stream of t's pod.
func (p *Pool) serveShareHint(t PodTarget, st *podwire.Stream) {
	hint := shareHintOf(t.Share)
	if hint == nil {
		podwire.RefuseSock(st, podwire.RefusedGate, "the instance's fleet share is not mounted from several nodes")
		return
	}
	roots := map[string]bool{}
	for _, r := range hint.Roots {
		roots[r.Name] = true
	}
	conn, err := podwire.AcceptSock(st)
	if err != nil {
		return
	}
	defer conn.Close()
	p.relayShareHints(t, roots, conn)
}

// relayShareHints passes the hints read from r to t's share's spoke, for
// the folders roots names, at most shareHintRate a second.
func (p *Pool) relayShareHints(t PodTarget, roots map[string]bool, rd io.Reader) {
	tree := ShareTree(t.Cluster, "", *t.Share)
	r := podwire.NewShareHintReader(rd)
	window, passed := time.Now(), 0
	for {
		m, err := r.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.opts.Logger.Warn("kube share: a pod's share hints", "fleet", t.Fleet, "instance", t.Instance, "err", err)
			}
			return
		}
		if !roots[m.Root] {
			continue
		}
		if now := time.Now(); now.Sub(window) >= time.Second {
			window, passed = now, 0
		}
		if passed >= shareHintRate {
			continue
		}
		passed++
		// No spoke attached, or its queue full: dropped (its scans find
		// the change).
		passHint(tree, sharesync.Hint{Root: m.Root, Paths: m.Paths})
	}
}
