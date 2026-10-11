//go:build !nok8s

package server

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	kubernetesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_prune.go is `fleet cluster gc`: the objects labelled for this daemon
// and connection that no instance record accounts for. They are only listed
// unless the caller applies, and even then a deletion needs this daemon to
// hold the connection's owner Lease (a restored copy of ~/.fleet elsewhere
// must not delete what the original still uses), and spares a PVC a live pod
// mounts, anything younger than an hour, and the objects of fleets that still
// exist (an instance created after a restored backup's snapshot), which go
// only when the caller names that fleet.

const (
	kubePruneTimeout = 2 * time.Minute
	// kubePruneMinAge spares objects a create may still be writing.
	kubePruneMinAge = time.Hour
)

// Prune actions, as PruneKubeClusterReply lists them.
const (
	pruneWouldDelete = fleetgrpc.KubePruneWouldDelete
	pruneDeleted     = fleetgrpc.KubePruneDeleted
	pruneYoung       = fleetgrpc.KubePruneYoung
	pruneMounted     = fleetgrpc.KubePruneMounted
	pruneFleetExists = fleetgrpc.KubePruneFleetExists
)

// pruneObject is one labelled object and what a prune makes of it.
type pruneObject struct {
	kind      string
	name      string
	labels    map[string]string
	createdAt time.Time
	mounted   bool
	action    string
	del       func(ctx context.Context) error
}

// PruneKubeCluster lists, and with apply deletes, the connection's
// unaccounted objects.
func (s *service) PruneKubeCluster(ctx context.Context, req *fleetgrpc.PruneKubeClusterRequest) (*fleetgrpc.PruneKubeClusterReply, error) {
	rec, ok := s.kube.record(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no Kubernetes connection %q", req.GetId())
	}
	id, err := s.kube.identity()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "this daemon's identity is unavailable: %v", err)
	}
	link, err := s.kube.connect(rec, s.kube.environ())
	if err != nil {
		_, msg := preflight.Classify(err)
		return nil, status.Errorf(codes.FailedPrecondition, "the connection does not load: %s", msg)
	}
	ctx, cancel := context.WithTimeout(ctx, kubePruneTimeout)
	defer cancel()

	lease := s.kube.leaseFor(rec, link, id)
	lst, err := lease.Ensure(ctx)
	if err != nil {
		_, msg := preflight.Classify(err)
		return nil, status.Errorf(codes.Unavailable, "claiming the owner Lease: %s", msg)
	}
	reply := &fleetgrpc.PruneKubeClusterReply{Owner: id.Owner(), LeaseHeld: lst.Held, LeaseHolder: lst.Holder, LeaseHolderHost: lst.HolderHost}
	if req.GetApply() && !lst.Held {
		return nil, status.Errorf(codes.FailedPrecondition, "owner_conflict: %s", preflight.OwnerConflictMessage(lst, s.kube.now()))
	}

	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	if f := req.GetFleet(); f != "" {
		if _, ok := st.Fleets[f]; !ok {
			return nil, status.Errorf(codes.NotFound, "fleet %q not found", f)
		}
	}
	objects, err := listPruneObjects(ctx, link.client, rec, id, st, req.GetFleet(), s.kube.now())
	if err != nil {
		_, msg := preflight.Classify(err)
		return nil, status.Errorf(codes.Unavailable, "listing the connection's objects: %s", msg)
	}
	for _, o := range objects {
		if req.GetApply() && o.action == pruneWouldDelete {
			// The Lease must still be ours at the moment of each deletion.
			if !lease.Holding() {
				return nil, status.Errorf(codes.FailedPrecondition, "owner_conflict: this daemon's owner Lease lapsed during the prune; %d deleted", reply.Deleted)
			}
			if err := o.del(ctx); err != nil {
				o.action = "failed: " + preflight.Message(err)
			} else {
				o.action = pruneDeleted
				reply.Deleted++
				flog.Info("kube: pruned", "cluster", rec.ID, "kind", o.kind, "name", o.name)
			}
		}
		po := &fleetgrpc.KubePruneObject{Kind: o.kind, Name: o.name, Mounted: o.mounted, Action: o.action}
		if !o.createdAt.IsZero() {
			po.CreatedAt = timestamppb.New(o.createdAt)
		}
		reply.Objects = append(reply.Objects, po)
	}
	return reply, nil
}

// leaseFor is the connection's owner Lease: the running worker's, which has
// watched the Lease's holder over time, else a fresh one.
func (m *kubeManager) leaseFor(rec fleet.KubeCluster, link *kubeLink, id owner.Identity) kubeLease {
	if w := m.worker(rec.ID); w != nil {
		w.mu.Lock()
		l := w.lease
		key := w.linkKey
		w.mu.Unlock()
		if l != nil && key == kubeConnectionKey(rec) {
			return l
		}
	}
	return m.newLease(link, rec, id)
}

// listPruneObjects lists every object labelled for this daemon and
// connection that no instance record accounts for, each with the action a
// prune takes (pruneWouldDelete when nothing spares it). fleetName is the
// fleet whose objects the caller wants gone too.
func listPruneObjects(ctx context.Context, cs kubernetes.Interface, rec fleet.KubeCluster, id owner.Identity, st *state.State, fleetName string, now time.Time) ([]*pruneObject, error) {
	ns := rec.Namespace
	opts := metav1.ListOptions{LabelSelector: names.Selector(id.Owner(), rec.ID)}
	propagation := metav1.DeletePropagationBackground
	delOpts := metav1.DeleteOptions{PropagationPolicy: &propagation}

	var objects []*pruneObject
	add := func(kind string, meta metav1.ObjectMeta, del func(ctx context.Context, name string, opts metav1.DeleteOptions) error) {
		name := meta.Name
		objects = append(objects, &pruneObject{
			kind: kind, name: name, labels: meta.Labels, createdAt: meta.CreationTimestamp.Time,
			del: func(ctx context.Context) error { return del(ctx, name, delOpts) },
		})
	}

	sts, err := cs.AppsV1().StatefulSets(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range sts.Items {
		add("sts", o.ObjectMeta, cs.AppsV1().StatefulSets(ns).Delete)
	}
	deploys, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range deploys.Items {
		add("deploy", o.ObjectMeta, cs.AppsV1().Deployments(ns).Delete)
	}
	jobs, err := cs.BatchV1().Jobs(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range jobs.Items {
		add("job", o.ObjectMeta, cs.BatchV1().Jobs(ns).Delete)
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range pods.Items {
		if len(o.OwnerReferences) == 0 { // the rest go with their controller
			add("pod", o.ObjectMeta, cs.CoreV1().Pods(ns).Delete)
		}
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range pvcs.Items {
		add("pvc", o.ObjectMeta, cs.CoreV1().PersistentVolumeClaims(ns).Delete)
	}
	secrets, err := cs.CoreV1().Secrets(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range secrets.Items {
		add("secret", o.ObjectMeta, cs.CoreV1().Secrets(ns).Delete)
	}
	cms, err := cs.CoreV1().ConfigMaps(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range cms.Items {
		add("configmap", o.ObjectMeta, cs.CoreV1().ConfigMaps(ns).Delete)
	}
	svcs, err := cs.CoreV1().Services(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range svcs.Items {
		add("svc", o.ObjectMeta, cs.CoreV1().Services(ns).Delete)
	}
	nps, err := cs.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, o := range nps.Items {
		add("netpol", o.ObjectMeta, cs.NetworkingV1().NetworkPolicies(ns).Delete)
	}
	// PodDisruptionBudgets exist only with protect_from_drain, and the
	// permission may be missing without it.
	if pdbs, err := cs.PolicyV1().PodDisruptionBudgets(ns).List(ctx, opts); err == nil {
		for _, o := range pdbs.Items {
			add("pdb", o.ObjectMeta, cs.PolicyV1().PodDisruptionBudgets(ns).Delete)
		}
	}

	// VolumeSnapshots are a clone's (of its source's disk), a CRD a cluster
	// may lack.
	snaps, err := kubernetesbackend.ListSnapshots(ctx, cs, ns, opts.LabelSelector)
	if err != nil {
		return nil, err
	}
	for _, o := range snaps {
		add("volumesnapshot", o, func(ctx context.Context, name string, opts metav1.DeleteOptions) error {
			return kubernetesbackend.DeleteSnapshot(ctx, cs, ns, name, opts)
		})
	}

	// Claims a live pod mounts, whoever's pod it is.
	mounted := map[string]bool{}
	allPods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, p := range allPods.Items {
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				mounted[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}

	known, fleets := accountedFor(st, rec, id)
	kept := objects[:0]
	for _, o := range objects {
		// The owner Lease and the managed pull Secrets belong to the
		// connection itself (the latter are deleted with it).
		if role := o.labels[names.LabelRole]; role == string(names.RoleOwner) || role == string(names.RolePullSecret) || known[o.labels[names.LabelKey]] {
			continue
		}
		o.mounted = o.kind == "pvc" && mounted[o.name]
		owningFleet := fleets[o.labels[names.LabelFleet]]
		switch {
		case owningFleet != "" && owningFleet != fleetName:
			o.action = pruneFleetExists
		case now.Sub(o.createdAt) < kubePruneMinAge:
			o.action = pruneYoung
		case o.mounted:
			o.action = pruneMounted
		default:
			o.action = pruneWouldDelete
		}
		kept = append(kept, o)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].kind != kept[j].kind {
			return pruneKindOrder(kept[i].kind) < pruneKindOrder(kept[j].kind)
		}
		return kept[i].name < kept[j].name
	})
	return kept, nil
}

// accountedFor reads what state.json knows on the connection: the keys of
// its instances, and the label value (FleetHash) of every fleet.
func accountedFor(st *state.State, rec fleet.KubeCluster, id owner.Identity) (keys map[string]bool, fleets map[string]string) {
	keys, fleets = map[string]bool{}, map[string]string{}
	for name, f := range st.Fleets {
		fleets[names.FleetHash(id.DaemonID, rec.ID, name)] = name
		for _, inst := range f.Instances {
			if inst.KubeCluster == rec.ID && inst.KubeKey != "" {
				keys[inst.KubeKey] = true
			}
		}
	}
	return keys, fleets
}

func pruneKindOrder(kind string) int {
	for i, k := range []string{"pvc", "sts", "deploy", "job", "pod", "svc", "netpol", "pdb", "secret", "configmap", "volumesnapshot"} {
		if k == kind {
			return i
		}
	}
	return 99
}

// kubeOrphans counts a connection's unaccounted objects for its
// capabilities: instance StatefulSets, fleet shares, cache volumes and the
// snapshots clones left.
func kubeOrphans(objects []*pruneObject) *fleetgrpc.KubeOrphans {
	o := &fleetgrpc.KubeOrphans{}
	for _, obj := range objects {
		switch {
		case obj.kind == "sts" && obj.labels[names.LabelRole] == string(names.RoleInstance):
			o.Instances++
		case obj.kind == "pvc" && strings.HasSuffix(obj.name, "-"+names.SuffixShare):
			o.Shares++
		case obj.kind == "pvc" && (strings.HasSuffix(obj.name, "-"+names.SuffixAptCache) || strings.HasSuffix(obj.name, "-"+names.SuffixImageCache) || strings.HasSuffix(obj.name, "-"+names.SuffixBuildkit)):
			o.CacheVolumes++
		case obj.kind == "volumesnapshot":
			o.SnapshotContents++
		}
	}
	return o
}
