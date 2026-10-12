//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// share_move.go moves a fleet's share to the connection's shared
// StorageClass once that changed: new fleets use the new class at once,
// and an existing fleet keeps its share (in the old class) until every
// instance of it on the connection is stopped, then moves with a one-time
// copy. The share keeps its name (every instance's StatefulSet mounts it
// by that name, and its pods are verified against a render that names
// it), so the move goes through a copy: a copy Job (jobboot's
// podseed.OpShareCopy, in the fleet's image, as one of its instances)
// copies the share whole onto a new volume of the new class (SuffixShareMove),
// the old share is deleted, a new share of the same name is made in the new
// class, and a second Job copies it back; then each instance is placed for
// the share's new mode (its StatefulSet's affinity, Codex's SQLite home for
// a multi-node share, the mode on fleetd's record of it) and the copy goes.
//
// How far a move got is on the cluster (AnnotationShareMove on the copy, and
// on the new share until it is filled), so a daemon that stops in between
// finishes it. Until the old share is deleted, a create or a start of one of
// the fleet's instances calls the move off (its copy deleted: the share is
// as it was, and moves the next time the fleet is stopped); after, it waits
// for the move, or (the move was a daemon's that stopped) is refused until
// the next pass finishes it (ShareMoveGate). One that passes the gate holds
// off any move of the share until it is done, so none begins between its
// gate and its pod.
//
// A single-node share moves to single-node or multi-node mode (the fleet's
// instances are then placed anywhere); a multi-node one only to a class
// that passes the multi-node test too, since its instances may have run on
// several nodes and their disks may be there.

// The phases a move records.
const (
	// moveCopying: the copy is being filled from the share.
	moveCopying = "copying"
	// moveCopied: the copy holds the share; the old one may go.
	moveCopied = "copied"
	// moveFilling (on the new share): it is being filled from the copy.
	moveFilling = "filling"
)

// shareMoveInfo is AnnotationShareMove's value.
type shareMoveInfo struct {
	V int `json:"v"`
	// From and To are the classes ("" the cluster's default).
	From string `json:"from"`
	To   string `json:"to"`
	// Mode is the mode the share takes in To; OldMode the one it had.
	Mode    topology.Mode `json:"mode"`
	OldMode topology.Mode `json:"old_mode"`
	Phase   string        `json:"phase"`
	// Node is where the first copy ran (a single-node share's copy, and so
	// the new share, are filled there).
	Node string `json:"node,omitempty"`
	// Size is the share's size.
	Size string `json:"size"`
	// Unused is the share's AnnotationVolumesUnused (named volumes its
	// upkeep is to remove), which the new share keeps.
	Unused string `json:"unused,omitempty"`
}

func readMoveInfo(meta metav1.ObjectMeta) (shareMoveInfo, bool) {
	raw, ok := meta.Annotations[names.AnnotationShareMove]
	if !ok {
		return shareMoveInfo{}, false
	}
	var m shareMoveInfo
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return shareMoveInfo{}, false
	}
	return m, true
}

func (m shareMoveInfo) annotation() string {
	m.V = 1
	data, _ := json.Marshal(m)
	return string(data)
}

// ShareMove is a fleet share that is not in the class new shares take.
type ShareMove struct {
	Fleet string
	// From and To are the classes ("" the cluster's default).
	From, To string
	// Mode is the mode the share takes in To.
	Mode topology.Mode
	// Begun: a move is under way (its copy exists); Committed: past the
	// point where it can be called off (the old share is deleted).
	Begun, Committed bool
	// Blocked says why it cannot move ("" when it can).
	Blocked string
	// NoInstance: the fleet has no instance on the connection to copy with
	// (its image runs the copy); it moves once one exists and is stopped.
	NoInstance bool
	// Untested says why it waits for the connection's next full preflight
	// ("" when it need not): the shared class changed since the last one,
	// so the mode it gives (the multi-node test) is not known yet.
	Untested string
}

// ShareMoveClaim is the copy a moving share goes through.
func ShareMoveClaim(fleetName, h8 string) string {
	return names.FleetObject(fleetName, h8, names.SuffixShareMove)
}

// shareCopyJob is the name of a move's copy Job (step 1: share → copy, 2:
// copy → new share).
func shareCopyJob(fleetName, h8 string, step int) string {
	return names.FleetObject(fleetName, h8, names.SuffixShareCopy+"-"+strconv.Itoa(step))
}

// shareCopySource is where a copy Job mounts what it copies.
const shareCopySource = "/fleet-share-src"

// Bounds of a move: one copy Job, and the waits for a deleted claim; how
// often its waits look (tests shorten them).
var (
	shareCopyTimeout  = time.Hour
	shareGoneWait     = 2 * time.Minute
	shareMoveInterval = DefaultWaitInterval
)

// The moves this daemon runs: a create or a start calls one off, or waits.
// holds counts, per share, the creates, clones, rebuilds and starts past
// the gate and not done yet: a move begins only while none is held.
var shareMoves = struct {
	mu    sync.Mutex
	runs  map[string]*shareMoveRun
	holds map[string]int
}{runs: map[string]*shareMoveRun{}, holds: map[string]int{}}

// holdShareLocked holds off a move of share key until the release it
// returns is called (once is enough; more do nothing). shareMoves.mu is
// held.
func holdShareLocked(key string) func() {
	shareMoves.holds[key]++
	return sync.OnceFunc(func() {
		shareMoves.mu.Lock()
		defer shareMoves.mu.Unlock()
		if shareMoves.holds[key]--; shareMoves.holds[key] <= 0 {
			delete(shareMoves.holds, key)
		}
	})
}

// shareMoveKey is the key of fleetName's share on connection clusterID in
// shareMoves (and the share's bind lock).
func shareMoveKey(daemonID, clusterID, fleetName string) string {
	return clusterID + "/" + ShareClaim(fleetName, names.FleetHash(daemonID, clusterID, fleetName))
}

type shareMoveRun struct {
	cancel    context.CancelCauseFunc
	done      chan struct{}
	mu        sync.Mutex
	committed bool
	// retired: the fleet's destroy ended the move (retire); postCancel
	// ends what runs past its point of no return.
	retired    bool
	postCancel context.CancelCauseFunc
}

// retire ends the run for the fleet's destroy, past its point of no return
// too: its new share is never made.
func (r *shareMoveRun) retire() {
	r.mu.Lock()
	r.retired = true
	post := r.postCancel
	r.mu.Unlock()
	r.cancel(ErrShareRetired)
	if post != nil {
		post(ErrShareRetired)
	}
}

// pastNoReturn is the context of the run's part past its point of no
// return: ctx without its cancellation (a create or a start no longer calls
// it off, a shutdown lets it finish), but for the fleet's destroy. stop
// releases it.
func (r *shareMoveRun) pastNoReturn(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	r.mu.Lock()
	r.postCancel = cancel
	retired := r.retired
	r.mu.Unlock()
	if retired {
		cancel(ErrShareRetired)
	}
	return ctx, func() { cancel(nil) }
}

func (r *shareMoveRun) commit() {
	r.mu.Lock()
	r.committed = true
	r.mu.Unlock()
}

func (r *shareMoveRun) isCommitted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.committed
}

// errShareMoveCalledOff is a move a create or a start called off.
var errShareMoveCalledOff = errors.New("called off: an instance of the fleet is starting")

// ErrShareRetired is a move the fleet's destroy ended (RetireShare): the
// share goes with the fleet, and no new one is made.
var ErrShareRetired = errors.New("ended: the fleet is being destroyed")

// fleetRecorded reports whether state.json still holds fleet name (one it
// cannot read is taken to): a move makes no claim for a fleet that is gone.
// A variable for tests.
var fleetRecorded = func(name string) bool {
	st, err := state.Load()
	if err != nil {
		return true
	}
	return st.Fleets[name] != nil
}

// moveScope is what a move works with.
type moveScope struct {
	p                  *Pool
	clusterID, ns      string
	fleetName, h8      string
	claim, copyClaim   string
	cs                 kclient.Interface
	ident              owner.Identity
	rec                fleet.KubeCluster
	caps               *fleetgrpc.KubeCapabilities
	share, copy        *corev1.PersistentVolumeClaim
	shareInfo, moveRec shareMoveInfo
	hasShareInfo       bool
	hasCopy            bool
	// dying is the share as deleted, while its deletion waits (a pod
	// still holds it); share is nil then.
	dying *corev1.PersistentVolumeClaim
}

// moveScopeOf reads fleetName's share and its move's copy on connection
// clusterID.
func (p *Pool) moveScopeOf(ctx context.Context, clusterID, fleetName string) (*moveScope, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, classify(err)
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return nil, err
	}
	return readMoveScope(ctx, p, cs, c.Namespace(), clusterID, currentRecord(c), ident, fleetName)
}

// readMoveScope is moveScopeOf on a client already at hand.
func readMoveScope(ctx context.Context, p *Pool, cs kclient.Interface, ns, clusterID string, rec fleet.KubeCluster, ident owner.Identity, fleetName string) (*moveScope, error) {
	h8 := names.FleetHash(ident.DaemonID, clusterID, fleetName)
	s := &moveScope{p: p, clusterID: clusterID, ns: ns, fleetName: fleetName, h8: h8, cs: cs, ident: ident, rec: rec,
		claim: ShareClaim(fleetName, h8), copyClaim: ShareMoveClaim(fleetName, h8)}
	pvcs := cs.CoreV1().PersistentVolumeClaims(s.ns)
	want := shareLabels(ident.Owner(), clusterID, h8)
	if pvc, err := pvcs.Get(ctx, s.claim, metav1.GetOptions{}); err == nil {
		switch {
		case notOurs(pvc.Labels, want) != "":
		case pvc.DeletionTimestamp != nil:
			s.dying = pvc
		default:
			s.share = pvc
			s.shareInfo, s.hasShareInfo = readMoveInfo(pvc.ObjectMeta)
		}
	} else if !gone(err) {
		return nil, classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	}
	if pvc, err := pvcs.Get(ctx, s.copyClaim, metav1.GetOptions{}); err == nil {
		if notOurs(pvc.Labels, want) == "" {
			s.copy = pvc
			s.moveRec, s.hasCopy = readMoveInfo(pvc.ObjectMeta)
		}
	} else if !gone(err) {
		return nil, classify(fmt.Errorf("reading the fleet share's copy %s: %w", s.copyClaim, err))
	}
	return s, nil
}

// classOf is a claim's class ("" when it names none).
func classOf(pvc *corev1.PersistentVolumeClaim) string {
	if pvc == nil || pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// defaultClass is the name of the cluster's default class ("" unknown).
func defaultClass(caps *fleetgrpc.KubeCapabilities) string {
	for _, sc := range caps.GetStorageClasses() {
		if sc.GetIsDefault() {
			return sc.GetName()
		}
	}
	return ""
}

// sameClass reports whether classes a and b are one, "" being the
// cluster's default.
func sameClass(a, b string, caps *fleetgrpc.KubeCapabilities) bool {
	if a == "" {
		a = defaultClass(caps)
	}
	if b == "" {
		b = defaultClass(caps)
	}
	return a == b
}

// moveTarget is the shared class existing shares move to on a connection
// ("", false when they stay where they are): the connection's shared class,
// else the one Auto picked for passing the multi-node test. A share Auto
// put in the instance disks' class (single-node mode, no class passed)
// follows no later change of theirs: it keeps its fleet together, wherever
// that is.
func moveTarget(rec fleet.KubeCluster, caps *fleetgrpc.KubeCapabilities) (string, bool) {
	switch {
	case rec.SharedStorageClass != "":
		return rec.SharedStorageClass, true
	case topology.Mode(caps.GetShareMode()) == topology.MultiNode && caps.GetSharedStorageClass() != "":
		return caps.GetSharedStorageClass(), true
	}
	return "", false
}

// ShareMoveOf is fleetName's share on connection clusterID when it is not
// in the class new shares take there, or its move is under way; nil when
// it is where it belongs (or the fleet has none there).
func (p *Pool) ShareMoveOf(ctx context.Context, clusterID, fleetName string) (*ShareMove, error) {
	s, err := p.moveScopeOf(ctx, clusterID, fleetName)
	if err != nil {
		return nil, err
	}
	return s.move(ctx)
}

func (s *moveScope) move(ctx context.Context) (*ShareMove, error) {
	if s.hasCopy || s.hasShareInfo {
		m := s.moveRec
		if !s.hasCopy {
			m = s.shareInfo
		}
		return &ShareMove{Fleet: s.fleetName, From: m.From, To: m.To, Mode: m.Mode, Begun: true,
			Committed: s.share == nil || s.hasShareInfo || classOf(s.share) == m.To && m.To != m.From}, nil
	}
	if s.share == nil {
		return nil, nil
	}
	facts, err := ConnectionFacts(ctx, s.clusterID)
	if err != nil {
		return nil, err
	}
	s.caps = facts.Capabilities
	caps := s.caps
	if caps == nil || topology.Mode(caps.GetShareMode()) == topology.Copy {
		return nil, nil
	}
	from := classOf(s.share)
	to, moves := moveTarget(s.rec, caps)
	if !moves || sameClass(from, to, caps) {
		return nil, nil
	}
	if picked := s.rec.SharedStorageClass; picked != "" && caps.GetSharedStorageClass() != picked && caps.GetShareRefused() == "" {
		// The last full preflight ran before the shared class changed: the
		// mode the class gives is not known until its multi-node test ran,
		// and a share moved now would stay in single-node mode there.
		return &ShareMove{Fleet: s.fleetName, From: from, To: to, Untested: "waits for the cluster test to judge " + to}, nil
	}
	old := topology.Mode(s.share.Annotations[names.AnnotationShareMode])
	if !old.Shared() {
		old = topology.SingleNode
	}
	mode := shareModeOf(caps, to)
	m := &ShareMove{Fleet: s.fleetName, From: from, To: to, Mode: mode}
	switch {
	case caps.GetShareRefused() != "":
		m.Blocked = caps.GetShareRefused()
	case old == topology.MultiNode && mode != topology.MultiNode:
		m.Blocked = fmt.Sprintf("its instances run on any node, and a share in %s is used from one node at a time (single-node mode): recreate the fleet's instances on %s to move it, or pick a shared class that passes the multi-node test",
			orDefault(to, "the default class"), clusterName(s.rec))
	case mode == topology.SingleNode:
		if why := s.immediateProblem(ctx, to); why != "" {
			m.Blocked = why
		}
	}
	if m.Blocked == "" {
		sets, err := s.instanceSets(ctx)
		if err != nil {
			return nil, err
		}
		m.NoInstance = len(sets) == 0
	}
	return m, nil
}

// immediateProblem is why a single-node share in class cannot keep the
// fleet together ("" when it can).
func (s *moveScope) immediateProblem(ctx context.Context, class string) string {
	var sc *fleetgrpc.KubeStorageClass
	for _, c := range s.caps.GetStorageClasses() {
		if (class != "" && c.GetName() == class) || (class == "" && c.GetIsDefault()) {
			sc = c
		}
	}
	if sc == nil {
		return ""
	}
	tc := topology.Class{Name: sc.GetName(), Immediate: sc.GetBindingMode() == string(storagev1.VolumeBindingImmediate), NodeLocal: sc.GetNodeLocal(), TopologyFree: sc.GetTopologyFree()}
	if tc.Immediate {
		if obj, err := s.cs.StorageV1().StorageClasses().Get(ctx, sc.GetName(), metav1.GetOptions{}); err == nil {
			tc.AllowedTopologies = obj.AllowedTopologies
		}
	}
	return topology.ImmediateProblem(tc, usableNodes(s.caps))
}

// instanceSets are the fleet's instance StatefulSets on the connection, by
// name.
func (s *moveScope) instanceSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	sel := names.Selector(s.ident.Owner(), s.clusterID) + "," + names.LabelShareGroup + "=" + s.h8 + "," + names.LabelRole + "=" + string(names.RoleInstance)
	l, err := s.cs.AppsV1().StatefulSets(s.ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, classify(fmt.Errorf("listing the fleet's instances: %w", err))
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items, nil
}

// ErrShareInUse is a move refused because an instance of the fleet still
// runs (or a pod still mounts its share).
var ErrShareInUse = errors.New("the fleet's share is in use")

// MoveShare moves fleetName's share on connection clusterID to the class
// new shares take there (ShareMoveOf), or finishes a move begun earlier.
// Every instance of the fleet there must be stopped: one that runs, or a
// pod that mounts the share, refuses it (ErrShareInUse). report hears its
// steps. A move of a share where it belongs is nothing.
func (p *Pool) MoveShare(ctx context.Context, clusterID, fleetName string, report func(string)) error {
	if report == nil {
		report = func(string) {}
	}
	s, err := p.moveScopeOf(ctx, clusterID, fleetName)
	if err != nil {
		return err
	}
	key := clusterID + "/" + s.claim
	run := &shareMoveRun{done: make(chan struct{})}
	ctx, run.cancel = context.WithCancelCause(ctx)
	defer run.cancel(nil)
	shareMoves.mu.Lock()
	if _, busy := shareMoves.runs[key]; busy {
		shareMoves.mu.Unlock()
		return nil
	}
	if shareMoves.holds[key] > 0 {
		shareMoves.mu.Unlock()
		return fmt.Errorf("%w: an instance of the fleet is being created or started", ErrShareInUse)
	}
	shareMoves.runs[key] = run
	shareMoves.mu.Unlock()
	defer func() {
		shareMoves.mu.Lock()
		delete(shareMoves.runs, key)
		shareMoves.mu.Unlock()
		close(run.done)
	}()
	// A create binding the share holds this too: a move never runs
	// beside one.
	release, err := lockShareBind(ctx, key)
	if err != nil {
		return err
	}
	defer release()

	// Read again under the lock.
	if s, err = p.moveScopeOf(ctx, clusterID, fleetName); err != nil {
		return err
	}
	if !fleetRecorded(fleetName) {
		return ErrShareRetired
	}
	m, err := s.move(ctx)
	if err != nil || m == nil || m.Untested != "" {
		return err
	}
	if m.Blocked != "" {
		return &kubeerr.Error{Code: kubeerr.Misconfigured.Narrow("share_move"), Err: fmt.Errorf("the fleet's share on %s cannot move to %s: %s", clusterName(s.rec), orDefault(m.To, "the default class"), m.Blocked)}
	}
	sets, err := s.instanceSets(ctx)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return nil
	}
	for _, set := range sets {
		if set.Spec.Replicas == nil || *set.Spec.Replicas > 0 {
			return fmt.Errorf("%w: instance %s runs", ErrShareInUse, set.Name)
		}
	}
	if pod := s.mountedBy(ctx); pod != "" {
		return fmt.Errorf("%w: pod %s mounts it", ErrShareInUse, pod)
	}
	if s.caps == nil {
		facts, err := ConnectionFacts(ctx, clusterID)
		if err != nil {
			return err
		}
		s.caps = facts.Capabilities
	}
	info := s.moveRec
	if !s.hasCopy {
		info = s.shareInfo
	}
	if !m.Begun {
		info = shareMoveInfo{From: m.From, To: m.To, Mode: m.Mode, OldMode: topology.Mode(s.share.Annotations[names.AnnotationShareMode]), Size: s.shareSize(),
			Unused: s.share.Annotations[names.AnnotationVolumesUnused]}
		if !info.OldMode.Shared() {
			info.OldMode = topology.SingleNode
		}
	}
	flog.Info("kube share: moving the fleet's share", "cluster", clusterID, "fleet", fleetName, "from", info.From, "to", info.To, "mode", info.Mode)
	src := &sets[0]
	if !m.Committed {
		report(fmt.Sprintf("copying the share to a %s volume", orDefault(info.To, "default-class")))
		if info, err = s.fillCopy(ctx, src, info); err != nil {
			s.callOff(info)
			if errors.Is(context.Cause(ctx), errShareMoveCalledOff) {
				return errShareMoveCalledOff
			}
			return err
		}
		if err := context.Cause(ctx); err != nil {
			s.callOff(info)
			return err
		}
		if !fleetRecorded(fleetName) {
			s.callOff(info)
			return ErrShareRetired
		}
		run.commit()
		report("deleting the share in " + orDefault(info.From, "the default class"))
		if err := s.deleteClaim(context.WithoutCancel(ctx), s.share); err != nil {
			return err
		}
		s.share = nil
	} else {
		run.commit()
		// An earlier pass deleted the old share, which a pod held then:
		// the new share takes its name only once it is gone.
		if s.share == nil && s.dying != nil {
			report("deleting the share in " + orDefault(info.From, "the default class"))
			if err := s.deleteClaim(context.WithoutCancel(ctx), s.dying); err != nil {
				return err
			}
		}
	}
	ctx, stop := run.pastNoReturn(ctx)
	defer stop()
	// A share already in the new class and no longer marked was filled by
	// an earlier pass that stopped before the end.
	if s.share == nil || s.hasShareInfo || !sameClass(classOf(s.share), info.To, s.caps) {
		// The fleet's destroy past the point of no return: the copy goes
		// with the fleet (RetireShare), no new share is made for it.
		if errors.Is(context.Cause(ctx), ErrShareRetired) || (s.share == nil && !fleetRecorded(fleetName)) {
			return ErrShareRetired
		}
		report("filling the new share")
		if err := s.fillShare(ctx, src, info); err != nil {
			if errors.Is(context.Cause(ctx), ErrShareRetired) {
				return ErrShareRetired
			}
			return err
		}
	}
	report("placing the fleet's instances")
	if err := s.replace(ctx, sets, info.Mode); err != nil {
		if errors.Is(context.Cause(ctx), ErrShareRetired) {
			return ErrShareRetired
		}
		return err
	}
	if err := s.deleteClaim(ctx, s.copy); err != nil {
		flog.Warn("kube share: deleting a moved share's copy", "cluster", clusterID, "fleet", fleetName, "claim", s.copyClaim, "err", err)
	}
	flog.Info("kube share: moved the fleet's share", "cluster", clusterID, "fleet", fleetName, "class", info.To, "mode", info.Mode)
	return nil
}

// shareSize is the share's size: what it holds now (it may have grown),
// and at least the connection's share size.
func (s *moveScope) shareSize() string {
	size := s.share.Spec.Resources.Requests[corev1.ResourceStorage]
	if c, ok := s.share.Status.Capacity[corev1.ResourceStorage]; ok && c.Cmp(size) > 0 {
		size = c
	}
	if q, err := resource.ParseQuantity(orDefault(s.rec.ShareSize, fleet.DefaultKubeShareSize)); err == nil && q.Cmp(size) > 0 {
		size = q
	}
	return size.String()
}

// mountedBy is a pod that mounts the share or its copy ("" when none), but
// the move's own copy Jobs' (one an earlier move left goes with its Job).
func (s *moveScope) mountedBy(ctx context.Context) string {
	l, err := s.cs.CoreV1().Pods(s.ns).List(ctx, metav1.ListOptions{LabelSelector: names.Selector(s.ident.Owner(), s.clusterID)})
	if err != nil {
		return ""
	}
	ours := map[string]bool{shareCopyJob(s.fleetName, s.h8, 1): true, shareCopyJob(s.fleetName, s.h8, 2): true}
	for _, pod := range l.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if ref := metav1.GetControllerOf(&pod); ref != nil && ref.Kind == "Job" && ours[ref.Name] {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if c := v.PersistentVolumeClaim; c != nil && (c.ClaimName == s.claim || c.ClaimName == s.copyClaim) {
				return pod.Name
			}
		}
	}
	return ""
}

// fillCopy makes the move's copy (again, when one an earlier move left is
// not complete) and fills it from the share.
func (s *moveScope) fillCopy(ctx context.Context, src *appsv1.StatefulSet, info shareMoveInfo) (shareMoveInfo, error) {
	if s.hasCopy && s.moveRec.Phase == moveCopied {
		return s.moveRec, nil
	}
	if s.copy != nil {
		if err := s.deleteClaim(ctx, s.copy); err != nil {
			return info, err
		}
	}
	info.Phase = moveCopying
	pvc, err := s.applyClaim(ctx, s.copyClaim, info, false)
	if err != nil {
		return info, err
	}
	s.copy = pvc
	node, err := s.copyJob(ctx, src, 1, s.copyClaim, s.claim, "")
	if err != nil {
		return info, fmt.Errorf("copying the share: %w", err)
	}
	info.Phase, info.Node = moveCopied, node
	if err := s.annotate(ctx, s.copyClaim, map[string]any{names.AnnotationShareMove: info.annotation()}); err != nil {
		return info, err
	}
	s.moveRec, s.hasCopy = info, true
	return info, nil
}

// callOff ends a move before its point of no return: its copy goes, and
// the share is as it was.
func (s *moveScope) callOff(info shareMoveInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	sc := s.scope()
	_, _ = sc.deleteAll(ctx, []objectMeta{{Kind: KindJob, Name: shareCopyJob(s.fleetName, s.h8, 1)}})
	if s.copy != nil {
		if err := s.deleteClaim(ctx, s.copy); err != nil {
			flog.Warn("kube share: deleting the copy of a move called off", "cluster", s.clusterID, "fleet", s.fleetName, "err", err)
		}
	}
	flog.Info("kube share: a share move was called off; the share is as it was", "cluster", s.clusterID, "fleet", s.fleetName, "to", info.To)
}

// fillShare makes the new share (unless an earlier move made it) and fills
// it from the copy, then records its topology.
func (s *moveScope) fillShare(ctx context.Context, src *appsv1.StatefulSet, info shareMoveInfo) error {
	if s.copy == nil {
		return kubeerr.New(kubeerr.Misconfigured.Narrow("share_move"), "the copy of the fleet's share (PVC %s) is gone mid-move: what the share held is on the daemon host's synced copy only; delete the fleet's share PVC %s to start it afresh", s.copyClaim, s.claim)
	}
	if s.share == nil {
		info.Phase = moveFilling
		pvc, err := s.applyClaim(ctx, s.claim, info, true)
		if err != nil {
			return err
		}
		s.share = pvc
	}
	node := ""
	if info.Mode == topology.SingleNode {
		node = info.Node
	}
	ran, err := s.copyJob(ctx, src, 2, s.claim, s.copyClaim, node)
	if err != nil {
		return fmt.Errorf("filling the new share: %w", err)
	}
	terms, err := s.shareTerms(ctx, info.Mode, ran)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(terms)
	return s.annotate(ctx, s.claim, map[string]any{names.AnnotationShareMove: nil, names.AnnotationShareTopology: string(data)})
}

// shareTerms is where the new share can be used: its volume's topology,
// else (single-node mode) the node its copy ran on.
func (s *moveScope) shareTerms(ctx context.Context, mode topology.Mode, node string) ([]corev1.NodeSelectorTerm, error) {
	pvc, err := s.cs.CoreV1().PersistentVolumeClaims(s.ns).Get(ctx, s.claim, metav1.GetOptions{})
	if err != nil {
		return nil, classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	}
	if name := pvc.Spec.VolumeName; name != "" {
		pv, err := s.cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			if terms := topology.VolumeTerms(pv); terms != nil || mode == topology.MultiNode {
				return orEmpty(terms), nil
			}
		case !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err):
			return nil, classify(fmt.Errorf("reading the fleet share's volume %s: %w", name, err))
		}
	}
	if mode == topology.MultiNode || node == "" {
		return []corev1.NodeSelectorTerm{}, nil
	}
	return []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}, nil
}

func orEmpty(t []corev1.NodeSelectorTerm) []corev1.NodeSelectorTerm {
	if t == nil {
		return []corev1.NodeSelectorTerm{}
	}
	return t
}

// applyClaim makes claim name in the move's class, at the share's size: the
// move's copy, or (share) the fleet's new share, marked with the move.
func (s *moveScope) applyClaim(ctx context.Context, name string, info shareMoveInfo, share bool) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(info.Size)
	if err != nil {
		return nil, fmt.Errorf("the share's size %q: %w", info.Size, err)
	}
	fleetInfo, _ := json.Marshal(struct {
		V     int    `json:"v"`
		Fleet string `json:"fleet"`
	}{1, s.fleetName})
	annotations := map[string]string{names.AnnotationInfo: string(fleetInfo), names.AnnotationShareMove: info.annotation()}
	if share {
		annotations[names.AnnotationShareMode] = string(info.Mode)
		if info.Unused != "" {
			annotations[names.AnnotationVolumesUnused] = info.Unused
		}
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.ns, Labels: shareLabels(s.ident.Owner(), s.clusterID, s.h8), Annotations: annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{shareAccess(info.Mode)},
			VolumeMode:  ptrTo(corev1.PersistentVolumeFilesystem),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if info.To != "" {
		pvc.Spec.StorageClassName = ptrTo(info.To)
	}
	body, err := podspec.ApplyBody(pvc)
	if err != nil {
		return nil, err
	}
	if _, err := applyObject(ctx, s.cs, s.ns, KindPVC, name, body); err != nil {
		return nil, classify(fmt.Errorf("making %s: %w", name, err))
	}
	got, err := s.cs.CoreV1().PersistentVolumeClaims(s.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, classify(fmt.Errorf("reading %s: %w", name, err))
	}
	return got, nil
}

// annotate sets (nil: removes) annotations of claim name.
func (s *moveScope) annotate(ctx context.Context, name string, annotations map[string]any) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if _, err := s.cs.CoreV1().PersistentVolumeClaims(s.ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		return classify(fmt.Errorf("recording the share move on %s: %w", name, err))
	}
	return nil
}

func (s *moveScope) scope() Scope {
	sc := Scope{Client: s.cs, Namespace: s.ns, Owner: s.ident.Owner(), Cluster: s.clusterID}
	if s.p != nil {
		sc.Gate, sc.Logger = s.p.gateFor(s.clusterID), s.p.opts.Logger
	}
	return sc
}

// deleteClaim deletes pvc (as it was read: its UID) and waits for it to go.
func (s *moveScope) deleteClaim(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc == nil {
		return nil
	}
	if _, err := s.scope().deleteAll(ctx, []objectMeta{metaOf(KindPVC, pvc)}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, shareGoneWait)
	defer cancel()
	for {
		got, err := s.cs.CoreV1().PersistentVolumeClaims(s.ns).Get(ctx, pvc.Name, metav1.GetOptions{})
		if gone(err) || (err == nil && got.UID != pvc.UID) {
			return nil
		}
		s.releaseClaim(ctx, pvc.Name)
		select {
		case <-ctx.Done():
			return fmt.Errorf("PVC %s was not gone after %s", pvc.Name, shareGoneWait)
		case <-time.After(shareMoveInterval):
		}
	}
}

// releaseClaim deletes the ended pods of the fleet's Jobs that mount claim
// (the seed that bound the share, a tool Job, a copy Job): the API server
// keeps a claim any pod on a node names (pvc-protection), ended or not,
// and a finished Job's pods stay until its TTL.
func (s *moveScope) releaseClaim(ctx context.Context, claim string) {
	releaseClaimHolders(ctx, s.scope(), claim)
}

// releaseClaimHolders deletes the ended pods of fleet's Jobs in sc that
// mount claim (releaseClaim).
func releaseClaimHolders(ctx context.Context, sc Scope, claim string) {
	l, err := sc.Client.CoreV1().Pods(sc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: names.Selector(sc.Owner, sc.Cluster)})
	if err != nil {
		return
	}
	var ended []objectMeta
	for i := range l.Items {
		pod := &l.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed || pod.DeletionTimestamp != nil {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if c := v.PersistentVolumeClaim; c != nil && c.ClaimName == claim {
				ended = append(ended, metaOf(KindPod, pod))
				break
			}
		}
	}
	if len(ended) > 0 {
		if _, err := sc.deleteAll(ctx, ended); err != nil {
			flog.Warn("kube share: deleting ended pods that hold a claim", "cluster", sc.Cluster, "claim", claim, "err", err)
		}
	}
}

// copyJob runs move step's copy Job, in the image of the fleet's instance
// src as it runs: dst filled from from (node, when set, pins it). It
// returns the node it ran on.
func (s *moveScope) copyJob(ctx context.Context, src *appsv1.StatefulSet, step int, dst, from, node string) (string, error) {
	gen := 1
	if g, err := strconv.Atoi(src.Annotations[names.AnnotationRebuildGen]); err == nil && names.ValidGeneration(g) {
		gen = g
	}
	name := shareCopyJob(s.fleetName, s.h8, step)
	in := podspec.CloneInput{
		Cluster: &s.rec, Namespace: s.ns, Name: dst, Key: src.Labels[names.LabelKey], Owner: s.ident.Owner(), ClusterID: s.clusterID,
		FleetName: s.fleetName, FleetHash: s.h8, InstanceName: src.Name,
	}
	job, err := podspec.CloneJob(src, in, gen, podspec.JobInput{
		Name: name, Claims: []podspec.JobClaim{{ClaimName: from, Path: shareCopySource, ReadOnly: true}},
		Resources: toolResources(), ActiveDeadline: shareCopyTimeout, Node: node,
	}, names.RoleTool)
	if err != nil {
		return "", err
	}
	sc := s.scope()
	if err := s.p.freshJob(ctx, sc, name); err != nil {
		return "", err
	}
	body, err := podspec.ApplyBody(job.Job)
	if err != nil {
		return "", err
	}
	applied, err := applyJob(ctx, s.cs, s.ns, body)
	if err != nil {
		return "", classify(err)
	}
	defer s.p.dropJob(sc, name)
	res, err := s.p.RunJob(ctx, JobRun{
		Cluster: s.clusterID, Job: name, PVC: dst, Interval: shareMoveInterval,
		Spec: podseed.JobSpec{V: podseed.JobSpecVersion, Op: podseed.OpShareCopy, Gen: 1, PVC: podspec.JobDiskPath, ShareCopy: &podseed.ShareCopySpec{Source: shareCopySource}},
	})
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", errors.New(res.Error)
	}
	ran := ""
	if pod, err := jobPod(ctx, s.cs, s.ns, applied); err == nil && pod != nil {
		ran = pod.Spec.NodeName
	}
	return ran, nil
}

// replace places each of the fleet's instances for its share in mode: its
// StatefulSet's affinity (as a create renders it: the share's placement),
// Codex's SQLite home on its own disk for a multi-node share, and the mode
// on fleetd's record of it.
func (s *moveScope) replace(ctx context.Context, sets []appsv1.StatefulSet, mode topology.Mode) error {
	pvc, err := s.cs.CoreV1().PersistentVolumeClaims(s.ns).Get(ctx, s.claim, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the fleet's share %s: %w", s.claim, err))
	}
	var terms []corev1.NodeSelectorTerm
	if raw := pvc.Annotations[names.AnnotationShareTopology]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &terms)
	}
	affinity := topology.Placement{Mode: mode, Terms: terms, ShareGroup: s.h8}.Affinity(true)
	var errs []error
	for i := range sets {
		if err := s.place(ctx, &sets[i], mode, affinity); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", sets[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

// place is one instance's part of replace.
func (s *moveScope) place(ctx context.Context, set *appsv1.StatefulSet, mode topology.Mode, affinity *corev1.Affinity) error {
	var ops []map[string]any
	switch {
	case affinity != nil:
		ops = append(ops, map[string]any{"op": "add", "path": "/spec/template/spec/affinity", "value": affinity})
	case set.Spec.Template.Spec.Affinity != nil:
		ops = append(ops, map[string]any{"op": "remove", "path": "/spec/template/spec/affinity"})
	}
	if len(ops) > 0 {
		patch, _ := json.Marshal(ops)
		if _, err := s.cs.AppsV1().StatefulSets(s.ns).Patch(ctx, set.Name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
			return classify(fmt.Errorf("placing its StatefulSet: %w", err))
		}
	}
	key := set.Labels[names.LabelKey]
	rec, err := LoadInstanceRecord(s.clusterID, key)
	if err != nil || rec.Share == nil {
		return nil
	}
	if topology.Mode(rec.Share.Mode) != mode {
		if err := s.codexHome(ctx, set, rec, mode); err != nil {
			return err
		}
		rec.Share.Mode = string(mode)
		if err := SaveInstanceRecord(rec); err != nil {
			return err
		}
	}
	return nil
}

// codexHome sets Codex's SQLite home on the instance's own disk in its exec
// spec when its share is multi-node and the fleet shares Codex's home
// (podspec.CodexSQLiteHomeEnv), and takes it out otherwise.
func (s *moveScope) codexHome(ctx context.Context, set *appsv1.StatefulSet, rec InstanceRecord, mode topology.Mode) error {
	codex := false
	for _, r := range rec.Share.Roots {
		codex = codex || r.Name == ".codex"
	}
	if !codex {
		return nil
	}
	name := names.SpecSecret(set.Name, rec.Gen)
	secrets := s.cs.CoreV1().Secrets(s.ns)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading its spec Secret %s: %w", name, err))
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(sec.Data[podspec.ExecFile], &spec); err != nil {
		return fmt.Errorf("its exec spec: %w", err)
	}
	home := map[string]string{}
	if raw, ok := spec["home_env"]; ok {
		_ = json.Unmarshal(raw, &home)
	}
	if mode == topology.MultiNode {
		home[podspec.CodexSQLiteHomeEnv] = podspec.CodexSQLiteHome
	} else {
		delete(home, podspec.CodexSQLiteHomeEnv)
	}
	if len(home) == 0 {
		delete(spec, "home_env")
	} else {
		raw, _ := json.Marshal(home)
		spec["home_env"] = raw
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	sec.Data[podspec.ExecFile] = data
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{FieldManager: DefaultFieldManager}); err != nil {
		return classify(fmt.Errorf("updating its spec Secret %s: %w", name, err))
	}
	return nil
}

// ShareMoveGate is what a create, clone, rebuild or start of an instance of
// fleetName on connection clusterID passes first: a move of the fleet's
// share this daemon runs is called off while it can be (its copy deleted,
// the share as it was), else waited for (within ctx); a move an earlier
// daemon left before its point of no return is called off the same way,
// and one past it refuses, until the next pass finishes it. Passed, it
// holds off any move of the share until release is called (once the
// instance's pod is up, or the job ends).
func (p *Pool) ShareMoveGate(ctx context.Context, clusterID, fleetName string) (release func(), err error) {
	ident, err := owner.LoadIdentity()
	if err != nil {
		return nil, err
	}
	return gateShare(ctx, shareMoveKey(ident.DaemonID, clusterID, fleetName), func() (*moveScope, error) {
		return p.moveScopeOf(ctx, clusterID, fleetName)
	})
}

// shareMoveGate is ShareMoveGate for a create, on its own client; the hold
// goes with the create (releaseMoveHolds).
func (c *creation) shareMoveGate(ctx context.Context) error {
	ident := owner.Identity{DaemonID: c.daemonID}
	release, err := gateShare(ctx, shareMoveKey(c.daemonID, c.id.cluster, c.id.fleet), func() (*moveScope, error) {
		return readMoveScope(ctx, c.p, c.cs, c.ns, c.id.cluster, c.rec, ident, c.id.fleet)
	})
	if err != nil {
		return err
	}
	c.moveHolds = append(c.moveHolds, release)
	return nil
}

// releaseMoveHolds lets go of the holds the create's gates took.
func (c *creation) releaseMoveHolds() {
	for _, release := range c.moveHolds {
		release()
	}
	c.moveHolds = nil
}

// gateShare is ShareMoveGate of share key, whose state read reads: it takes
// the hold first (a move this daemon runs is called off or waited for
// before), so what it then reads no move changes until release.
func gateShare(ctx context.Context, key string, read func() (*moveScope, error)) (func(), error) {
	var release func()
	for release == nil {
		shareMoves.mu.Lock()
		run := shareMoves.runs[key]
		if run == nil {
			release = holdShareLocked(key)
		}
		shareMoves.mu.Unlock()
		if run == nil {
			break
		}
		if !run.isCommitted() {
			run.cancel(errShareMoveCalledOff)
		}
		select {
		case <-run.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the fleet's share to move: %w", ctx.Err())
		}
	}
	s, err := read()
	if err != nil {
		release()
		return nil, err
	}
	if !s.hasCopy && !s.hasShareInfo {
		return release, nil
	}
	m, _ := s.move(ctx)
	if m != nil && !m.Committed {
		s.callOff(s.moveRec)
		return release, nil
	}
	release()
	to := s.moveRec.To
	if !s.hasCopy {
		to = s.shareInfo.To
	}
	return nil, &kubeerr.Error{Code: kubeerr.Unavailable.Narrow("share_moving"),
		Err:  fmt.Errorf("the fleet's share on %s is moving to StorageClass %s", clusterName(s.rec), orDefault(to, "default")),
		Hint: "try again once the move finishes (a minute or so; fleet cluster ls names the fleets still moving)"}
}

// retireGate is what a fleet's destroy passes before it retires the share of
// key: a move of it this daemon runs is ended, past its point of no return
// too (retire), and waited for within ctx; until release, no move begins.
func retireGate(ctx context.Context, key string) (func(), error) {
	for {
		shareMoves.mu.Lock()
		run := shareMoves.runs[key]
		if run == nil {
			release := holdShareLocked(key)
			shareMoves.mu.Unlock()
			return release, nil
		}
		shareMoves.mu.Unlock()
		run.retire()
		select {
		case <-run.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the move of the fleet's share to end: %w", ctx.Err())
		}
	}
}

// RetireShareCopy deletes the copy a share move left of fleetName's share on
// connection clusterID (a destroyed fleet's); nothing when there is none.
func (p *Pool) RetireShareCopy(ctx context.Context, clusterID, fleetName string) error {
	s, err := p.moveScopeOf(ctx, clusterID, fleetName)
	if err != nil || s.copy == nil {
		return err
	}
	return s.deleteClaim(ctx, s.copy)
}
