//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// spoke.go is fleetd's end of an instance pod's spoke: the process in the
// pod (internal/podshare, as the share uid) that keeps the fleet's share, or
// on the copy tier the instance's own copy, in sync with the daemon host's
// hub. Its control connection is a TagSyncCtl stream on the pod's agent
// session, which starts it; its content over sharesync.InlineMax travels on
// TagSyncBulk streams on the bulk session. One spoke runs per share: the
// pods of a fleet's share elect the oldest running one (ElectSpoke), and the
// share's lease (a lock on the share, whose epoch counts its spokes) keeps a
// second from running beside it; fleetd refuses a spoke whose epoch is older
// than one it already saw for the same tree (it lost the lease).

// spokeLeases are the newest lease epoch fleetd saw for each tree (by
// connection and the share's PVC, or instance key on the copy tier).
var spokeLeases = struct {
	mu     sync.Mutex
	newest map[string]uint64
}{newest: map[string]uint64{}}

// acceptLease refuses a lease epoch older than the newest one seen for tree,
// and records a newer one.
func acceptLease(tree string, lease uint64) error {
	spokeLeases.mu.Lock()
	defer spokeLeases.mu.Unlock()
	if have := spokeLeases.newest[tree]; lease < have {
		return fmt.Errorf("the spoke's lease (epoch %d) is older than one this daemon saw on the same folders (epoch %d)", lease, have)
	}
	spokeLeases.newest[tree] = lease
	return nil
}

// ShareTree names the tree an instance's spoke keeps: the fleet's share
// (shared by its pods), or the instance's own copy on the copy tier.
func ShareTree(cluster, key string, share ShareRecord) string {
	if share.Claim != "" && topology.Mode(share.Mode).Shared() {
		return cluster + "/" + share.Claim
	}
	return cluster + "/" + key
}

// ShareSide is the hub's name for an instance's spoke: one side per share
// (whichever of its pods runs the spoke), or per instance on the copy tier.
// Its location is the connection: logins stay per location.
func ShareSide(cluster, key string, share ShareRecord) sharesync.SideInfo {
	return sharesync.SideInfo{ID: "k8s:" + ShareTree(cluster, key, share), Location: cluster}
}

// SpokeLink is the connection to an instance pod's spoke.
type SpokeLink struct {
	// Ctl is the spoke's control connection.
	Ctl  net.Conn
	sess *AgentSession
	tree string
}

// OpenBulk opens one of the spoke's bulk connections: a TagSyncBulk stream
// on the pod's bulk session (opened on first use).
func (l *SpokeLink) OpenBulk(ctx context.Context) (net.Conn, error) {
	b, err := l.sess.Bulk(ctx)
	if err != nil {
		return nil, err
	}
	return b.OpenRelay(ctx, podwire.TagSyncBulk, struct{}{})
}

// Close ends the spoke's control connection, and with it the spoke.
func (l *SpokeLink) Close() error { return l.Ctl.Close() }

// OpenSpoke starts the spoke of instance pod t, whose share share is (its
// record's), and returns its connection. The pod gets its agent session
// first (verified, as for anything fleetd sends a pod).
func (p *Pool) OpenSpoke(ctx context.Context, t PodTarget, key string, share ShareRecord) (*SpokeLink, error) {
	tree, err := p.leaseTree(ctx, t.Cluster, key, share)
	if err != nil {
		return nil, err
	}
	sess, err := p.EnsureAgent(ctx, t)
	if err != nil {
		return nil, err
	}
	ctl, err := sess.Client().OpenRelay(ctx, podwire.TagSyncCtl, share.SpokeSpec(string(sess.Pod().UID)))
	if err != nil {
		return nil, fmt.Errorf("starting the share's spoke in pod %s: %w", t.Pod(), err)
	}
	return &SpokeLink{Ctl: ctl, sess: sess, tree: tree}, nil
}

// leaseTree names the tree whose lease epochs a spoke's go by: its
// ShareTree, and for a share the PVC's UID too. The lease lives on the
// share, so a share made again under the same name (its fleet destroyed and
// made again, the PVC deleted by hand) counts its epochs from 1 again, and
// would be refused as older than the old share's.
func (p *Pool) leaseTree(ctx context.Context, clusterID, key string, share ShareRecord) (string, error) {
	tree := ShareTree(clusterID, key, share)
	if share.Claim == "" || !topology.Mode(share.Mode).Shared() {
		return tree, nil
	}
	c, err := p.Cluster(clusterID)
	if err != nil {
		return "", err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return "", err
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(c.Namespace()).Get(ctx, share.Claim, metav1.GetOptions{})
	if err != nil {
		return "", classify(fmt.Errorf("reading the fleet's share %s: %w", share.Claim, err))
	}
	return tree + "@" + string(pvc.UID), nil
}

// ServeSpoke serves link's spoke on hub as the side info names (fleetd's
// own record of whose tree it is, never what the spoke says) until its
// connection ends or ctx does (sharesync.ServeRemote), its bulk content on
// the pod's bulk session; a spoke with a stale lease is refused.
func (p *Pool) ServeSpoke(ctx context.Context, link *SpokeLink, hub sharesync.RemoteHub, info sharesync.SideInfo, attached func(lease uint64)) error {
	defer link.Close()
	return sharesync.ServeRemote(ctx, link.Ctl, hub, info, sharesync.RemoteOptions{
		OpenBulk: link.OpenBulk,
		Accept:   func(lease uint64) error { return acceptLease(link.tree, lease) },
		Attached: func(lease uint64) {
			flog.Info("kube share: the spoke is attached", "side", info.ID, "pod", link.sess.Pod().Name, "lease", lease)
			if attached != nil {
				attached(lease)
			}
		},
		Logger: p.opts.Logger,
	})
}

// SpokeCandidate is one instance pod of a share that could run its spoke.
type SpokeCandidate struct {
	// StatefulSet is the instance's.
	StatefulSet string
	// Started is when the pod started.
	Started time.Time
	// Key is the instance's key (its pod's LabelKey).
	Key string
}

// ElectSpoke picks the pod of a share that runs its spoke: the oldest
// running one (by start time, then name), which the share's lease keeps
// alone; false without one.
func ElectSpoke(cands []SpokeCandidate) (SpokeCandidate, bool) {
	if len(cands) == 0 {
		return SpokeCandidate{}, false
	}
	sorted := slices.Clone(cands)
	slices.SortFunc(sorted, func(a, b SpokeCandidate) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.StatefulSet, b.StatefulSet)
	})
	return sorted[0], true
}

// ShareCandidates are the running, ready instance pods of the fleet whose
// share-group is h8 on connection clusterID that mount claim: the pods
// ElectSpoke chooses from.
func (p *Pool) ShareCandidates(ctx context.Context, clusterID, h8, claim string) ([]SpokeCandidate, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, err
	}
	owner, err := p.owner()
	if err != nil {
		return nil, err
	}
	sel := names.Selector(owner, clusterID) + "," + names.LabelShareGroup + "=" + h8 + "," + names.LabelRole + "=" + string(names.RoleInstance)
	l, err := cs.CoreV1().Pods(c.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, classify(err)
	}
	var out []SpokeCandidate
	for i := range l.Items {
		pod := &l.Items[i]
		if !podReady(pod) || !mountsClaim(pod, claim) {
			continue
		}
		sts, ok := cutPodSuffix(pod.Name)
		if !ok {
			continue
		}
		started := pod.CreationTimestamp.Time
		if pod.Status.StartTime != nil {
			started = pod.Status.StartTime.Time
		}
		out = append(out, SpokeCandidate{StatefulSet: sts, Started: started, Key: pod.Labels[names.LabelKey]})
	}
	return out, nil
}

// mountsClaim reports whether pod mounts claim.
func mountsClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// ErrNoShare is an instance without fleet mounts: it has no spoke.
var ErrNoShare = errors.New("the instance has no fleet mounts on its cluster")

// InstanceShare is the share of the instance with key on connection
// clusterID, as its record keeps it, with the pod to reach.
func InstanceShare(clusterID, key string) (PodTarget, ShareRecord, error) {
	rec, err := LoadInstanceRecord(clusterID, key)
	if err != nil {
		return PodTarget{}, ShareRecord{}, err
	}
	if rec.Share == nil {
		return PodTarget{}, ShareRecord{}, ErrNoShare
	}
	return rec.target(), *rec.Share, nil
}
