//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// share_maint.go is the upkeep of a fleet's share on a connection: how full
// it is (measured in a running pod of the fleet that has its agent session,
// so the upkeep never starts one), growing a nearly full one by half where
// its StorageClass allows and the connection auto-expands (as cache volumes
// and instance disks do), else warning once in fleet.log, with the line its
// Storage section and `fleet sync status` show (ShareUsage). Agents'
// transcripts and session folders grow without bound, and a full share
// pauses the fleet's sync on the cluster (sharesync's "share full").

// shareWarned are the shares whose nearly-full warning fleet.log has had.
var shareWarned = struct {
	mu   sync.Mutex
	keys map[string]bool
}{keys: map[string]bool{}}

// ShareUsage is fleetName's share on connection clusterID; ok is false when
// the fleet has none there (no fleet mounts, or the copy tier).
func (p *Pool) ShareUsage(ctx context.Context, clusterID, fleetName string) (fleet.ShareUsage, bool, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return fleet.ShareUsage{}, false, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return fleet.ShareUsage{}, false, err
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return fleet.ShareUsage{}, false, err
	}
	h8 := names.FleetHash(ident.DaemonID, clusterID, fleetName)
	claim := ShareClaim(fleetName, h8)
	pvc, err := cs.CoreV1().PersistentVolumeClaims(c.Namespace()).Get(ctx, claim, metav1.GetOptions{})
	if gone(err) {
		return fleet.ShareUsage{}, false, nil
	}
	if err != nil {
		return fleet.ShareUsage{}, false, classify(err)
	}
	rec := currentRecord(c)
	if why := notOurs(pvc.Labels, shareLabels(ident.Owner(), clusterID, h8)); why != "" {
		return fleet.ShareUsage{}, false, nil
	}
	u := fleet.ShareUsage{Cluster: clusterID, Location: clusterName(rec), Fleet: fleetName, Claim: claim}
	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	if capacity.IsZero() {
		capacity = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	u.CapacityBytes = capacity.Value()
	class := ""
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	u.Expandable = rec.AutoExpandsDisk() && p.classExpands(ctx, clusterID, class)
	if used, ok := shareMeasure(p, ctx, clusterID, h8, claim); ok {
		u.UsedBytes, u.Measured = used, true
	}
	if u.Percent() >= fleet.ShareFullPercent && !u.Expandable {
		u.Warning = fmt.Sprintf("%d%% full; ", u.Percent())
		if rec.AutoExpandsDisk() {
			u.Warning += "its StorageClass cannot grow it: free room in the fleet's agent dirs or custom mounts, or use an expandable shared storage class"
		} else {
			u.Warning += "auto-expand is off on " + clusterName(rec)
		}
	}
	return u, true, nil
}

// MaintainShare is the upkeep of fleetName's share on connection clusterID:
// a share ShareFullPercent full grows by half where it can, else it warns
// (once until it is not full any more). It returns the share's usage.
func (p *Pool) MaintainShare(ctx context.Context, clusterID, fleetName string) (fleet.ShareUsage, bool, error) {
	u, ok, err := p.ShareUsage(ctx, clusterID, fleetName)
	if err != nil || !ok {
		return u, ok, err
	}
	key := clusterID + "/" + u.Claim
	switch {
	case u.Percent() >= fleet.ShareFullPercent && u.Expandable:
		if err := p.growShare(ctx, clusterID, u.Claim); err != nil {
			flog.Warn("kube share: growing a nearly full share", "cluster", clusterID, "fleet", fleetName, "claim", u.Claim, "err", err)
		}
	case u.Warning != "":
		shareWarned.mu.Lock()
		first := !shareWarned.keys[key]
		shareWarned.keys[key] = true
		shareWarned.mu.Unlock()
		if first {
			flog.Warn("kube share: the fleet's share is nearly full", "cluster", clusterID, "fleet", fleetName, "claim", u.Claim, "warning", u.Warning)
		}
	default:
		shareWarned.mu.Lock()
		delete(shareWarned.keys, key)
		shareWarned.mu.Unlock()
	}
	return u, true, nil
}

// growShare grows a share by half (to a whole Gi), unless a growth asked for
// earlier has not happened yet.
func (p *Pool) growShare(ctx context.Context, clusterID, claim string) error {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return err
	}
	pvcs := cs.CoreV1().PersistentVolumeClaims(c.Namespace())
	pvc, err := pvcs.Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return err
	}
	have := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok && capacity.Cmp(have) < 0 {
		return nil
	}
	next := grownBy(have)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"resources": map[string]any{"requests": map[string]string{"storage": next.String()}}}})
	if _, err := pvcs.Patch(ctx, claim, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		return err
	}
	flog.Info("kube share: grew a nearly full share", "cluster", clusterID, "claim", claim, "from", have.String(), "to", next.String())
	return nil
}

// grownBy is q grown by half, rounded up to a whole Gi.
func grownBy(q resource.Quantity) resource.Quantity {
	const gi = int64(1) << 30
	n := q.Value() + q.Value()/2
	n = (n + gi - 1) / gi * gi
	return *resource.NewQuantity(n, resource.BinarySI)
}

// classExpands reports whether class grows its volumes, as the connection's
// last preflight found it.
func (p *Pool) classExpands(ctx context.Context, clusterID, class string) bool {
	f, err := currentEnvironment().facts(ctx, clusterID)
	if err != nil {
		return false
	}
	for _, sc := range f.Capabilities.GetStorageClasses() {
		if (class != "" && sc.GetName() == class) || (class == "" && sc.GetIsDefault()) {
			return sc.GetAllowExpansion()
		}
	}
	return false
}

// shareMeasure measures a share (a seam for tests).
var shareMeasure = (*Pool).measureShare

// measureShare is how many bytes the share's folders hold: du, as root, in
// a running pod of the fleet that mounts it and already has its agent
// session (none: not measured).
func (p *Pool) measureShare(ctx context.Context, clusterID, h8, claim string) (int64, bool) {
	cands, err := p.ShareCandidates(ctx, clusterID, h8, claim)
	if err != nil {
		return 0, false
	}
	for _, cand := range cands {
		// The pod's labels name its record: a pod some other hand made with
		// fleet's labels names none of ours.
		if cand.Key == "" {
			continue
		}
		rec, err := LoadInstanceRecord(clusterID, cand.Key)
		if err != nil || rec.StatefulSet != cand.StatefulSet {
			continue
		}
		sess := p.Session(rec.target())
		if sess == nil {
			continue
		}
		res, err := sess.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: []string{"du", "-sxk", podshare.MountDir + "/" + podshare.RootsDir}}}, nil)
		if err != nil || res.Code != 0 {
			continue
		}
		if n, ok := parseDuKB(res.Stdout); ok {
			return n, true
		}
	}
	return 0, false
}

// parseDuKB reads `du -sk`'s kilobytes as bytes.
func parseDuKB(out []byte) (int64, bool) {
	f := strings.Fields(string(bytes.TrimSpace(out)))
	if len(f) == 0 {
		return 0, false
	}
	kb, err := strconv.ParseInt(f[0], 10, 64)
	return kb * 1024, err == nil
}
