//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// share_retire.go is what becomes of a fleet's share on a connection when
// the fleet is destroyed (the daemon host keeps the canonical copy): it is
// deleted once the daemon flushed what it held, or kept, labelled an orphan
// (names.LabelOrphan), when the flush failed, so nothing the host never got
// is lost: Settings counts it, `fleet cluster gc --apply` deletes it, and a
// fleet made again under the same name takes it up again. A connection whose
// share_retention is retain keeps it unlabelled.

// RetireShare deletes fleetName's share on connection clusterID, or with
// keep labels it an orphan; one still labelled an orphan (an earlier fleet
// of the name kept it, and this one never took it up) is never deleted. It
// returns the share's name ("" when the fleet has none there, or the PVC of
// that name is not this daemon's).
func (p *Pool) RetireShare(ctx context.Context, clusterID, fleetName string, keep bool) (string, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return "", err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return "", classify(err)
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return "", err
	}
	h8 := names.FleetHash(ident.DaemonID, clusterID, fleetName)
	claim := ShareClaim(fleetName, h8)
	pvcs := cs.CoreV1().PersistentVolumeClaims(c.Namespace())
	pvc, err := pvcs.Get(ctx, claim, metav1.GetOptions{})
	if gone(err) {
		return "", nil
	}
	if err != nil {
		return "", classify(fmt.Errorf("reading the fleet's share %s: %w", claim, err))
	}
	if why := notOurs(pvc.Labels, shareLabels(ident.Owner(), clusterID, h8)); why != "" {
		flog.Warn("kube share: a PVC named as the destroyed fleet's share is not this daemon's; left alone", "cluster", clusterID, "fleet", fleetName, "claim", claim, "why", why)
		return "", nil
	}
	if !keep && pvc.Labels[names.LabelOrphan] != "" {
		// An earlier fleet of this name left it, its last changes never on
		// the daemon host, and this one never took it up: this fleet's
		// flush says nothing of what it holds.
		flog.Info("kube share: left the share an earlier fleet of this name kept as an orphan", "cluster", clusterID, "fleet", fleetName, "claim", claim)
		return claim, nil
	}
	if keep {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{names.LabelOrphan: names.OrphanFlushFailed}}})
		if _, err := pvcs.Patch(ctx, claim, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
			return claim, classify(fmt.Errorf("labelling the fleet's share %s an orphan: %w", claim, err))
		}
		flog.Warn("kube share: kept the destroyed fleet's share, labelled an orphan: its last changes did not reach the daemon host", "cluster", clusterID, "fleet", fleetName, "claim", claim)
		return claim, nil
	}
	propagation := metav1.DeletePropagationBackground
	uid := pvc.UID
	if err := pvcs.Delete(ctx, claim, metav1.DeleteOptions{PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !gone(err) {
		return claim, classify(fmt.Errorf("deleting the fleet's share %s: %w", claim, err))
	}
	flog.Info("kube share: deleted the destroyed fleet's share", "cluster", clusterID, "fleet", fleetName, "claim", claim)
	return claim, nil
}

// adoptShare drops the orphan label of a share a fleet takes up again.
// Best effort: a share left labelled is counted in Settings and kept by the
// fleet's destroy (RetireShare), which only costs a `fleet cluster gc`.
func (c *creation) adoptShare(ctx context.Context, claim string) {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{names.LabelOrphan: nil}}})
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Patch(ctx, claim, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		flog.Warn("kube share: taking up an orphaned share", "cluster", c.id.cluster, "fleet", c.id.fleet, "claim", claim, "err", err)
		return
	}
	flog.Info("kube share: the fleet took up the share it left", "cluster", c.id.cluster, "fleet", c.id.fleet, "claim", claim)
}

// ElectShareSpoke is the instance whose pod runs the spoke of the share the
// instance with key on connection clusterID mounts (share, its record's):
// on the copy tier the instance itself, while its pod runs; for a share its
// fleet's pods mount, the oldest running one (ElectSpoke) whose pod names a
// record of fleetd's. "" when no pod of the share runs.
func (p *Pool) ElectShareSpoke(ctx context.Context, clusterID, fleetName, key string, share ShareRecord) (string, error) {
	if share.Claim == "" || !topology.Mode(share.Mode).Shared() {
		return p.copySpoke(ctx, clusterID, key)
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return "", err
	}
	cands, err := p.ShareCandidates(ctx, clusterID, names.FleetHash(ident.DaemonID, clusterID, fleetName), share.Claim)
	if err != nil {
		return "", err
	}
	var ours []SpokeCandidate
	for _, cand := range cands {
		if cand.Key == "" {
			continue
		}
		rec, err := LoadInstanceRecord(clusterID, cand.Key)
		if err != nil || rec.StatefulSet != cand.StatefulSet || rec.Share == nil || rec.Share.Claim != share.Claim {
			continue
		}
		ours = append(ours, cand)
	}
	elected, ok := ElectSpoke(ours)
	if !ok {
		return "", nil
	}
	return elected.Key, nil
}

// copySpoke is key when the pod of the copy-tier instance with key runs
// ("" when it does not: a stopped instance's copy on its disk has nothing
// that can change, and nothing to serve it).
func (p *Pool) copySpoke(ctx context.Context, clusterID, key string) (string, error) {
	rec, err := LoadInstanceRecord(clusterID, key)
	if err != nil {
		return "", err
	}
	c, err := p.Cluster(clusterID)
	if err != nil {
		return "", err
	}
	objs, err := p.objects(c)
	if err != nil {
		return "", classify(err)
	}
	pod, err := objs.Pod(ctx, c.Namespace(), rec.target().Pod())
	if gone(err) {
		return "", nil
	}
	if err != nil {
		return "", classify(err)
	}
	if !podReady(pod) {
		return "", nil
	}
	return key, nil
}

// ServeShareSpoke runs the spoke of the share the instance with key mounts
// in that instance's pod, attached to hub as info, until its connection or
// ctx ends (Pool.OpenSpoke, Pool.ServeSpoke). roots, when given, are the
// folders it keeps in place of the instance's own: those of every instance
// that mounts the share, so the one pod that runs its spoke keeps them all.
func (p *Pool) ServeShareSpoke(ctx context.Context, clusterID, key string, roots []podshare.SpokeRoot, hub sharesync.RemoteHub, info sharesync.SideInfo, attached func(lease uint64)) error {
	target, share, err := InstanceShare(clusterID, key)
	if err != nil {
		return err
	}
	if len(roots) > 0 {
		share.Roots = roots
	}
	link, err := p.OpenSpoke(ctx, target, key, share)
	if err != nil {
		return err
	}
	return p.ServeSpoke(ctx, link, hub, info, attached)
}
