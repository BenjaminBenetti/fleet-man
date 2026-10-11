//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// share.go is a fleet's share on a connection: the volume its instances'
// fleet mounts (the agents' state, the shared files, the custom mounts) live
// on, one PVC per (connection, fleet), fs-<fleet>-<h8>-share, every instance
// pod of the fleet mounts by subPath (podspec/share.go).
//
// Its mode, fixed when the share is made (AnnotationShareMode), places the
// fleet's pods (internal/k8s/topology). In single-node mode (no StorageClass
// passed the multi-node share test, which comes with the StorageClass
// picker) the share is in the connection's shared class, else the instance
// disks' class, ReadWriteOnce, and every pod of the fleet runs where it can
// be used: the first instance's seed mounts it beside its own disk, so a
// WaitForFirstConsumer class binds both on one node, and from then on the
// share volume's topology (recorded on the PVC, AnnotationShareTopology, as
// reading a PV takes a cluster-scoped permission) is a required node affinity
// on all of them. A class that binds Immediately is refused there unless the
// cluster is one node, or the class pins one zone. When the deep test found
// that two pods on one node cannot mount one ReadWriteOnce volume, the fleet
// has no share at all (the copy tier): each instance keeps its own copy on
// its disk.
//
// The share outlives its instances; destroying the fleet deletes it once the
// daemon flushed what it held, else keeps it labelled an orphan
// (share_retire.go). It grows by half when 90% full where its class allows
// (MaintainShare).

// shareState is where a create puts the instance's fleet mounts.
type shareState struct {
	mode  topology.Mode
	claim string
	class string
	// layout is the tree's layout, owned by the share uid.
	layout podshare.Layout
	// placement places the fleet's pods; its Terms are nil until the share
	// is bound (and for a volume every node reaches).
	placement topology.Placement
	// selinuxMount: the share's CSI driver mounts with an SELinux context,
	// and the API server takes the policy (podspec.SELinuxMountSupported).
	selinuxMount bool
	// bind: the share is unbound, and this create's seed binds it beside
	// its disk, holding the fleet's bind lock until done (release).
	bind    bool
	release func()
	// seedNode is the node the binding seed's pod ran on.
	seedNode string
}

// seedClaims are the disks the seed mounts beside the instance's: the share
// when it binds it.
func (s *shareState) seedClaims() []podspec.JobClaim {
	if s == nil || !s.bind {
		return nil
	}
	return []podspec.JobClaim{{ClaimName: s.claim, Path: shareSeedPath}}
}

// shareSeedPath is where a binding seed mounts the share (it never writes
// there).
const shareSeedPath = "/fleet-share"

// input is the render's view of the share.
func (s *shareState) input() *podspec.Share {
	if s == nil {
		return nil
	}
	claim := s.claim
	if s.mode == topology.Copy {
		claim = ""
	}
	return &podspec.Share{Claim: claim, Layout: s.layout}
}

// placementFor is the render's placement.
func (s *shareState) placementFor() *topology.Placement {
	if s == nil || s.mode == topology.Copy {
		return nil
	}
	p := s.placement
	return &p
}

// done releases the fleet's bind lock, if held.
func (s *shareState) done() {
	if s != nil && s.release != nil {
		s.release()
		s.release = nil
	}
}

// record is the share as the instance record keeps it.
func (s *shareState) record() *ShareRecord {
	if s == nil {
		return nil
	}
	return &ShareRecord{
		Mode: string(s.mode), Claim: s.input().Claim,
		Roots: podshare.LayoutRoots(s.layout.Entries, syncpolicy.Defaults().Root),
		UID:   s.layout.UID, GID: s.layout.GID,
	}
}

// ShareClaim is the name of fleetName's share PVC on the connection whose
// fleet hash is h8 (names.FleetHash).
func ShareClaim(fleetName, h8 string) string {
	return names.FleetObject(fleetName, h8, names.SuffixShare)
}

// shareLabels are the labels of a fleet's share.
func shareLabels(owner, clusterID, h8 string) map[string]string {
	l := names.Base(owner, clusterID)
	l[names.LabelFleet] = h8
	l[names.LabelRole] = string(names.RoleShare)
	l[names.LabelShareGroup] = h8
	return l
}

// layoutOf is the tree a request's fleet mounts need: the resolver's Layout
// it carries, else (a caller that has none: Up without a context) each
// fleet-shared mount's folder.
func layoutOf(req backend.UpRequest) []backend.LayoutEntry {
	if len(req.Layout) > 0 {
		return req.Layout
	}
	var out []backend.LayoutEntry
	for _, m := range req.Mounts {
		if m.Scope == backend.MountScopeFleetShared && m.SubPath != "" {
			out = append(out, backend.LayoutEntry{Path: m.SubPath, Dir: true, Mode: 0o777})
		}
	}
	return out
}

// podLayout is entries as the share-layout init container takes them, owned
// by uid:gid.
func podLayout(entries []backend.LayoutEntry, uid, gid int) podshare.Layout {
	l := podshare.Layout{V: podshare.LayoutVersion, UID: uid, GID: gid}
	for _, e := range entries {
		l.Entries = append(l.Entries, podshare.Entry{Path: path.Clean(e.Path), Dir: e.Dir, Mode: uint32(e.Mode.Perm()), Seed: e.Seed})
	}
	return l
}

// shareModeOf is the mode a new share takes on a connection whose preflight
// found caps: the copy tier when the deep test found two pods on one node
// cannot mount one RWO volume, the mode the StorageClass picker's test chose
// (multi-node) when it did, else single-node.
func shareModeOf(caps *fleetgrpc.KubeCapabilities) topology.Mode {
	switch m := topology.Mode(caps.GetShareMode()); m {
	case topology.Copy, topology.MultiNode:
		return m
	}
	return topology.SingleNode
}

// The fleets whose share is being bound by a create: a second create waits,
// so it finds the share bound and placed instead of racing the first to it.
var shareBinds = struct {
	mu   sync.Mutex
	busy map[string]chan struct{}
}{busy: map[string]chan struct{}{}}

// lockShareBind takes the bind lock of a share, waiting within ctx.
func lockShareBind(ctx context.Context, key string) (func(), error) {
	for {
		shareBinds.mu.Lock()
		ch, held := shareBinds.busy[key]
		if !held {
			ch = make(chan struct{})
			shareBinds.busy[key] = ch
			shareBinds.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					shareBinds.mu.Lock()
					delete(shareBinds.busy, key)
					shareBinds.mu.Unlock()
					close(ch)
				})
			}, nil
		}
		shareBinds.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the fleet's first instance on the cluster to bind its share: %w", ctx.Err())
		}
	}
}

// prepareShare decides where the instance's fleet mounts live, and makes
// the fleet's share ready for its render: ensured, and placed when it is
// bound; otherwise this create's seed binds it (holding the fleet's bind
// lock until finishShare). A request without fleet mounts has no share.
func (c *creation) prepareShare(ctx context.Context) error {
	entries := layoutOf(c.req)
	if len(entries) == 0 {
		return nil
	}
	uid, gid := shareIDs()
	if c.identity.NonRoot {
		uid, gid = -1, -1
	}
	h8 := names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet)
	s := &shareState{claim: ShareClaim(c.id.fleet, h8), layout: podLayout(entries, uid, gid), placement: topology.Placement{ShareGroup: h8}}
	if err := s.layout.Validate(); err != nil {
		return fmt.Errorf("the fleet's shared mounts: %w", err)
	}
	pvcs := c.cs.CoreV1().PersistentVolumeClaims(c.ns)
	pvc, err := pvcs.Get(ctx, s.claim, metav1.GetOptions{})
	switch {
	case gone(err):
		s.mode = shareModeOf(c.facts.Capabilities)
		if s.mode == topology.Copy {
			c.share = s
			return nil
		}
		s.class = c.shareClass()
		// Only one node's pods mount a single-node share: one an Immediate
		// class put elsewhere than the instances' disks cannot be used. A
		// multi-node share is mounted from any node.
		if s.mode == topology.SingleNode {
			if err := c.checkBinding(ctx, s.class); err != nil {
				return err
			}
		}
		if pvc, err = c.applyShare(ctx, s, h8); err != nil {
			return err
		}
	case err != nil:
		return classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	default:
		if why := notOurs(pvc.Labels, shareLabels(c.owner, c.id.cluster, h8)); why != "" {
			return kubeerr.New(kubeerr.Misconfigured, "a PVC named %s, which would be the fleet's share, is in namespace %s but is not this daemon's (%s); remove it or use another namespace", s.claim, c.ns, why)
		}
		s.mode = topology.Mode(pvc.Annotations[names.AnnotationShareMode])
		if !s.mode.Shared() {
			s.mode = topology.SingleNode
		}
		if pvc.Labels[names.LabelOrphan] != "" {
			// A share its destroyed fleet left (its last changes never
			// reached the daemon host) is the fleet's again: they come back
			// with it.
			c.adoptShare(ctx, pvc.Name)
		}
		if pvc.Spec.StorageClassName != nil {
			s.class = *pvc.Spec.StorageClassName
		}
	}
	s.placement.Mode = s.mode
	s.selinuxMount = c.selinuxMountFor(s.class)
	c.share = s
	if s.mode == topology.SingleNode {
		// The instance's disk binds where its pod lands, which must be where
		// the share can be used.
		if err := c.checkBinding(ctx, c.rec.StorageClass); err != nil {
			return err
		}
	}
	return c.placeShare(ctx, pvc)
}

// placeShare places the fleet's pods by its share pvc: by the topology its
// bound volume has, or (unbound) by what binds it: this create's seed, with
// the fleet's bind lock held, for a class that binds when the first pod is
// scheduled; the class itself, waited for, for one that binds at once.
func (c *creation) placeShare(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	s := c.share
	if pvc.Status.Phase == corev1.ClaimBound {
		return c.learnShareTopology(ctx, pvc)
	}
	key := c.id.cluster + "/" + s.claim
	backend.ReportStep(ctx, fleet.StepApplyManifests, "waiting for the fleet's share to bind")
	release, err := lockShareBind(ctx, key)
	if err != nil {
		return err
	}
	pvc, err = c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, s.claim, metav1.GetOptions{})
	if err != nil {
		release()
		return classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	}
	if pvc.Status.Phase == corev1.ClaimBound {
		release()
		return c.learnShareTopology(ctx, pvc)
	}
	if c.bindsImmediately(s.class) {
		defer release()
		pvc, err = c.waitShareBound(ctx, s.claim)
		if err != nil {
			return err
		}
		return c.learnShareTopology(ctx, pvc)
	}
	s.bind, s.release = true, release
	return nil
}

// finishShare is the seed done: a share it bound is placed now (its bound
// volume's topology, else the node the seed ran on), and the fleet's bind
// lock goes. A seed that failed leaves the share unbound for the next create.
func (c *creation) finishShare(ctx context.Context, seeded bool) error {
	s := c.share
	if s == nil || !s.bind {
		return nil
	}
	defer s.done()
	s.bind = false
	if !seeded {
		return nil
	}
	pvc, err := c.waitShareBound(ctx, s.claim)
	if err != nil {
		return err
	}
	return c.learnShareTopology(ctx, pvc)
}

// shareBindWait bounds the wait for a share to bind once something that
// binds it ran.
const shareBindWait = 2 * time.Minute

// waitShareBound waits for the share claim to bind.
func (c *creation) waitShareBound(ctx context.Context, claim string) (*corev1.PersistentVolumeClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, shareBindWait)
	defer cancel()
	for {
		pvc, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, claim, metav1.GetOptions{})
		if err == nil && pvc.Status.Phase == corev1.ClaimBound {
			return pvc, nil
		}
		if err != nil && ctx.Err() == nil {
			return nil, classify(fmt.Errorf("reading the fleet's share %s: %w", claim, err))
		}
		select {
		case <-ctx.Done():
			detail := "it did not bind"
			if pvc != nil {
				detail = pvcDetail(pvc)
			}
			return nil, &DeadlineError{Kind: CodePVCPending, Wait: "the fleet's share (PVC " + claim + ") to bind", After: shareBindWait, Detail: detail}
		case <-time.After(DefaultWaitInterval):
		}
	}
}

// learnShareTopology fills the placement from the bound share pvc: the
// topology recorded on it, else its volume's (recorded then), else the node
// a pod mounting it runs on (recorded then; a node-local volume's, and a
// tighter bound than a zone for a network one).
func (c *creation) learnShareTopology(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	s := c.share
	if raw, ok := pvc.Annotations[names.AnnotationShareTopology]; ok {
		var terms []corev1.NodeSelectorTerm
		if err := json.Unmarshal([]byte(raw), &terms); err == nil {
			s.placement.Terms = terms
			return nil
		}
	}
	terms, known := []corev1.NodeSelectorTerm(nil), false
	if name := pvc.Spec.VolumeName; name != "" {
		pv, err := c.cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			terms, known = topology.VolumeTerms(pv), true
		case !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err):
			return classify(fmt.Errorf("reading the fleet share's volume %s: %w", name, err))
		}
	}
	if !known {
		node := s.seedNode
		if node == "" {
			node = c.shareHolderNode(ctx, pvc.Name)
		}
		if node == "" {
			// Nothing tells: the pods still go beside running holders.
			flog.Warn("kube share: the share's topology is unknown; only running pods that mount it place the next", "cluster", c.id.cluster, "fleet", c.id.fleet, "claim", pvc.Name)
			return nil
		}
		terms, known = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}, true
	}
	if terms == nil {
		terms = []corev1.NodeSelectorTerm{}
	}
	s.placement.Terms = terms
	data, _ := json.Marshal(terms)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{names.AnnotationShareTopology: string(data)}}})
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Patch(ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		flog.Warn("kube share: recording the share's topology", "cluster", c.id.cluster, "claim", pvc.Name, "err", err)
	}
	flog.Info("kube share: placed the fleet by its share", "cluster", c.id.cluster, "fleet", c.id.fleet, "claim", pvc.Name, "topology", string(data))
	return nil
}

// shareHolderNode is the node a pod of the fleet that mounts claim runs on
// ("" when none is scheduled).
func (c *creation) shareHolderNode(ctx context.Context, claim string) string {
	h8 := names.FleetHash(c.daemonID, c.id.cluster, c.id.fleet)
	l, err := c.cs.CoreV1().Pods(c.ns).List(ctx, metav1.ListOptions{LabelSelector: names.Selector(c.owner, c.id.cluster) + "," + names.LabelShareGroup + "=" + h8})
	if err != nil {
		return ""
	}
	for _, p := range l.Items {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				return p.Spec.NodeName
			}
		}
	}
	return ""
}

// applyShare makes the fleet's share in s's class, the connection's share
// size, recording its mode.
func (c *creation) applyShare(ctx context.Context, s *shareState, h8 string) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(orDefault(c.rec.ShareSize, fleet.DefaultKubeShareSize))
	if err != nil {
		return nil, kubeerr.New(kubeerr.Misconfigured, "share_size %q on %s is not a Kubernetes quantity: %v", c.rec.ShareSize, clusterName(c.rec), err)
	}
	info, _ := json.Marshal(struct {
		V     int    `json:"v"`
		Fleet string `json:"fleet"`
	}{1, c.id.fleet})
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name: s.claim, Namespace: c.ns, Labels: shareLabels(c.owner, c.id.cluster, h8),
			Annotations: map[string]string{names.AnnotationInfo: string(info), names.AnnotationShareMode: string(s.mode)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{shareAccess(s.mode)},
			VolumeMode:  ptrTo(corev1.PersistentVolumeFilesystem),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if s.class != "" {
		pvc.Spec.StorageClassName = ptrTo(s.class)
	}
	body, err := podspec.ApplyBody(pvc)
	if err != nil {
		return nil, err
	}
	if _, err := applyObject(ctx, c.cs, c.ns, KindPVC, s.claim, body); err != nil {
		return nil, classify(fmt.Errorf("making the fleet's share %s: %w", s.claim, err))
	}
	flog.Info("kube share: made the fleet's share", "cluster", c.id.cluster, "fleet", c.id.fleet, "claim", s.claim, "class", s.class, "size", size.String(), "mode", s.mode)
	got, err := c.cs.CoreV1().PersistentVolumeClaims(c.ns).Get(ctx, s.claim, metav1.GetOptions{})
	if err != nil {
		return nil, classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	}
	return got, nil
}

// shareAccess is the access mode of a share of mode m: one node's pods
// mount a single-node share, any node's a multi-node one.
func shareAccess(m topology.Mode) corev1.PersistentVolumeAccessMode {
	if m == topology.MultiNode {
		return corev1.ReadWriteMany
	}
	return corev1.ReadWriteOnce
}

// shareClass is the class a new share takes: the connection's shared class,
// else (single-node mode) the instance disks' ("" being the cluster's
// default).
func (c *creation) shareClass() string {
	if c.rec.SharedStorageClass != "" {
		return c.rec.SharedStorageClass
	}
	return c.rec.StorageClass
}

// classFacts is what the preflight found of class ("" the default).
func (c *creation) classFacts(class string) *fleetgrpc.KubeStorageClass {
	for _, sc := range c.facts.Capabilities.GetStorageClasses() {
		if (class != "" && sc.GetName() == class) || (class == "" && sc.GetIsDefault()) {
			return sc
		}
	}
	return nil
}

// bindsImmediately reports a class whose volumes bind when made.
func (c *creation) bindsImmediately(class string) bool {
	sc := c.classFacts(class)
	return sc != nil && sc.GetBindingMode() == string(storagev1.VolumeBindingImmediate)
}

// selinuxMountFor reports whether pods mounting a volume of class get the
// SELinux mount option instead of a relabel.
func (c *creation) selinuxMountFor(class string) bool {
	sc := c.classFacts(class)
	return sc != nil && sc.GetSelinuxMount() && podspec.SELinuxMountSupported(c.facts.Capabilities.GetVersion())
}

// checkBinding refuses a class that binds Immediately where a fleet's
// volumes cannot then be kept together (topology.ImmediateProblem). Without
// the preflight's word on the class nothing is refused.
func (c *creation) checkBinding(ctx context.Context, class string) error {
	sc := c.classFacts(class)
	if sc == nil {
		return nil
	}
	tc := topology.Class{Name: sc.GetName(), Immediate: sc.GetBindingMode() == string(storagev1.VolumeBindingImmediate), NodeLocal: sc.GetNodeLocal()}
	if tc.Immediate {
		if obj, err := c.cs.StorageV1().StorageClasses().Get(ctx, sc.GetName(), metav1.GetOptions{}); err == nil {
			tc.AllowedTopologies = obj.AllowedTopologies
		}
	}
	if why := topology.ImmediateProblem(tc, len(c.facts.Capabilities.GetNodes())); why != "" {
		return &kubeerr.Error{Code: kubeerr.Misconfigured.Narrow("share_binding"), Err: errors.New(why), Hint: topology.ImmediateFix}
	}
	return nil
}

// notOurs says why an object's labels are not those want names ("" when
// they are).
func notOurs(have, want map[string]string) string {
	for _, k := range []string{names.LabelManagedBy, names.LabelOwner, names.LabelCluster, names.LabelFleet, names.LabelRole} {
		if have[k] != want[k] {
			if have[k] == "" {
				return "it has no " + k + " label"
			}
			return fmt.Sprintf("its %s label is %q", k, have[k])
		}
	}
	return ""
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func ptrTo[T any](v T) *T { return &v }

func clusterName(rec fleet.KubeCluster) string {
	if rec.DisplayName != "" {
		return rec.DisplayName
	}
	return rec.ID
}
