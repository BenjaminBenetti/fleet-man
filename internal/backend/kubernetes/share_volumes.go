//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// share_volumes.go is the end of the fleet's named volumes on its share.
// Docker removes a named volume when the last container that mounted it goes
// (the devcontainer backend's Down); so does a cluster: when an instance is
// destroyed, each of the fleet's named volumes it mounted that no other
// instance of the fleet on the connection mounts (no instance record names
// it, no create in flight is filling it) is removed from the share, through a
// running pod of the share. With none running it is marked on the share
// (AnnotationVolumesUnused), for the share's upkeep to remove once one runs;
// a create that names it first takes it fresh (its seed empties it), as
// docker makes a removed volume anew. On the copy tier each instance's
// volumes are on its own disk, and go with it.

// volumeRetireTimeout bounds a destroy's removal of its named volumes: the
// rest of the teardown must not wait on a slow pod.
const volumeRetireTimeout = 30 * time.Second

// volumeUse holds, per share (cluster/claim), the volumes the creates in
// flight claim (their seeds fill them before any record names them), and
// the lock that keeps a removal and a claim apart.
var volumeUse = struct {
	mu     sync.Mutex
	shares map[string]*shareVolumes
}{shares: map[string]*shareVolumes{}}

// shareVolumes is one share's claims.
type shareVolumes struct {
	mu       sync.Mutex
	inFlight map[string]int
}

func volumesOf(clusterID, claim string) *shareVolumes {
	key := clusterID + "/" + claim
	volumeUse.mu.Lock()
	defer volumeUse.mu.Unlock()
	sv := volumeUse.shares[key]
	if sv == nil {
		sv = &shareVolumes{inFlight: map[string]int{}}
		volumeUse.shares[key] = sv
	}
	return sv
}

// claimVolumes claims ids (folders in the share's volumes folder) for a
// create in flight, until release, and takes their unused marks off. It
// returns those a destroy left unused (marked, and no instance mounts them):
// the create's seed takes them fresh.
func claimVolumes(ctx context.Context, cs kclient.Interface, ns, clusterID, claim string, ids []string) (fresh map[string]bool, release func()) {
	sv := volumesOf(clusterID, claim)
	sv.mu.Lock()
	defer sv.mu.Unlock()
	use, useErr := volumesInUse(sv, clusterID, claim, "")
	for _, id := range ids {
		sv.inFlight[id]++
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			sv.mu.Lock()
			defer sv.mu.Unlock()
			for _, id := range ids {
				if sv.inFlight[id]--; sv.inFlight[id] <= 0 {
					delete(sv.inFlight, id)
				}
			}
		})
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return nil, release
	}
	unused := unusedVolumes(pvc)
	var keep []string
	var taken map[string]bool
	for _, id := range unused {
		if !slices.Contains(ids, id) {
			keep = append(keep, id)
			continue
		}
		if taken == nil {
			taken = map[string]bool{}
		}
		taken[id] = useErr == nil && !use[id]
	}
	if taken == nil {
		return nil, release
	}
	if err := markUnused(ctx, cs, ns, claim, keep); err != nil {
		// Still marked: the upkeep may yet remove it under this create's
		// seed's feet. Taken as it is instead.
		flog.Warn("kube share: taking a destroyed instance's named volume back", "cluster", clusterID, "claim", claim, "err", err)
		return nil, release
	}
	for id, f := range taken {
		if f {
			if fresh == nil {
				fresh = map[string]bool{}
			}
			fresh[id] = true
		}
	}
	return fresh, release
}

// unusedVolumes is the share's AnnotationVolumesUnused list.
func unusedVolumes(pvc *corev1.PersistentVolumeClaim) []string {
	var out []string
	for _, id := range strings.Split(pvc.Annotations[names.AnnotationVolumesUnused], ",") {
		if podshare.ValidVolumeID(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// markUnused sets the share's AnnotationVolumesUnused to ids (none removes
// it).
func markUnused(ctx context.Context, cs kclient.Interface, ns, claim string, ids []string) error {
	var val any
	if len(ids) > 0 {
		slices.Sort(ids)
		val = strings.Join(ids, ",")
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{names.AnnotationVolumesUnused: val}}})
	_, err := cs.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, claim, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager})
	return err
}

// volumesInUse are the named volumes on share claim of connection clusterID
// that an instance other than except mounts (its record names them) or a
// create in flight claims. Called with the share's claims locked.
func volumesInUse(sv *shareVolumes, clusterID, claim, except string) (map[string]bool, error) {
	use := map[string]bool{}
	for id := range sv.inFlight {
		use[id] = true
	}
	recs, err := instanceRecords(clusterID)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if rec.Key == except || rec.Share == nil || rec.Share.Claim != claim {
			continue
		}
		for _, id := range rec.Share.Volumes {
			use[id] = true
		}
	}
	return use, nil
}

// instanceRecords are fleetd's records of connection clusterID's instances.
// One that cannot be read fails it: a volume an unread record names may be
// in use.
func instanceRecords(clusterID string) ([]InstanceRecord, error) {
	dir := state.KubeInstancesDir(clusterID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the connection's instance records: %w", err)
	}
	var out []InstanceRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading instance record %s: %w", e.Name(), err)
		}
		var rec InstanceRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("instance record %s: %w", e.Name(), err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// RetireVolumes is the end of the named volumes instance rec mounted on its
// fleet's share, as the instance is destroyed: those no other instance there
// mounts are marked unused on the share and removed now through a running
// pod of the share when one has its agent (else by the share's upkeep).
// Best-effort: logged.
func (p *Pool) RetireVolumes(ctx context.Context, rec InstanceRecord) {
	s := rec.Share
	if s == nil || s.Claim == "" || len(s.Volumes) == 0 {
		return
	}
	c, err := p.Cluster(rec.Cluster)
	if err != nil {
		return
	}
	cs, err := p.clientset(c)
	if err != nil {
		return
	}
	sv := volumesOf(rec.Cluster, s.Claim)
	sv.mu.Lock()
	use, err := volumesInUse(sv, rec.Cluster, s.Claim, rec.Key)
	if err != nil {
		sv.mu.Unlock()
		flog.Warn("kube down: keeping the instance's named volumes", "cluster", rec.Cluster, "key", rec.Key, "err", err)
		return
	}
	var gone []string
	for _, id := range s.Volumes {
		if !use[id] {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		sv.mu.Unlock()
		return
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(c.Namespace()).Get(ctx, s.Claim, metav1.GetOptions{})
	if err == nil {
		err = markUnused(ctx, cs, c.Namespace(), s.Claim, append(unusedVolumes(pvc), gone...))
	}
	sv.mu.Unlock()
	if err != nil {
		flog.Warn("kube down: marking the instance's named volumes unused", "cluster", rec.Cluster, "claim", s.Claim, "volumes", gone, "err", classify(err))
		return
	}
	flog.Info("kube down: the instance was the last to mount named volumes of its fleet", "cluster", rec.Cluster, "claim", s.Claim, "volumes", gone)
	ident, err := owner.LoadIdentity()
	if err != nil {
		return
	}
	p.sweepVolumes(ctx, rec.Cluster, names.FleetHash(ident.DaemonID, rec.Cluster, rec.Fleet), s.Claim, rec.Key)
}

// sweepVolumes removes the named volumes marked unused on share claim that
// no instance but except (one being destroyed) mounts by now, through a
// running pod of the share that has its agent, and takes their marks off. A
// marked volume something mounts keeps its mark (the destroyed instance's
// record may not be gone yet): a create that mounts it again takes it off.
func (p *Pool) sweepVolumes(ctx context.Context, clusterID, h8, claim, except string) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return
	}
	cs, err := p.clientset(c)
	if err != nil {
		return
	}
	sv := volumesOf(clusterID, claim)
	sv.mu.Lock()
	defer sv.mu.Unlock()
	pvc, err := cs.CoreV1().PersistentVolumeClaims(c.Namespace()).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return
	}
	unused := unusedVolumes(pvc)
	if len(unused) == 0 {
		return
	}
	use, err := volumesInUse(sv, clusterID, claim, except)
	if err != nil {
		return
	}
	var gone, left []string
	for _, id := range unused {
		if use[id] {
			left = append(left, id)
		} else {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 || !volumeRemover(p, ctx, clusterID, h8, claim, gone) {
		return
	}
	flog.Info("kube share: removed named volumes no instance mounts", "cluster", clusterID, "claim", claim, "volumes", gone)
	if err := markUnused(ctx, cs, c.Namespace(), claim, left); err != nil {
		flog.Warn("kube share: recording the named volumes left to remove", "cluster", clusterID, "claim", claim, "err", classify(err))
	}
}

// volumeRemover removes named volumes from a share (a seam for tests).
var volumeRemover = (*Pool).removeVolumes

// removeVolumes removes the named volumes ids from share claim: rm, as root,
// in a running pod of the fleet that mounts it and already has its agent
// session. It reports whether one did.
func (p *Pool) removeVolumes(ctx context.Context, clusterID, h8, claim string, ids []string) bool {
	cands, err := p.ShareCandidates(ctx, clusterID, h8, claim)
	if err != nil {
		return false
	}
	argv := []string{"rm", "-rf", "--"}
	for _, id := range ids {
		argv = append(argv, path.Join(podshare.MountDir, podseed.SharedVolumesDir, id))
	}
	for _, cand := range cands {
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
		res, err := sess.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: argv}}, nil)
		if err == nil && res.Code == 0 {
			return true
		}
		if err == nil {
			err = fmt.Errorf("rm exited %d: %s", res.Code, strings.TrimSpace(string(res.Stderr)))
		}
		flog.Warn("kube share: removing named volumes in a pod of the share", "cluster", clusterID, "claim", claim, "pod", cand.StatefulSet, "err", err)
	}
	return false
}
