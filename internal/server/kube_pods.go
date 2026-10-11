//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_pods.go is fleetd's side of its Kubernetes instances' pods, driven by
// state changes and the pool's informer events, with or without a TUI:
//
//   - it tells the pool which instances to watch (every Kubernetes record)
//     and to keep an agent up in (the running ones whose backend handle
//     names their pod), at the priority fleetd's state gives them;
//   - after a restart the cluster made on its own (an OOM kill, a crash, an
//     eviction, a drain, a lost node, a deleted pod) it re-runs the
//     instance's post-start work once the pod runs again, and shows
//     "sessions reset: <cause>" on the instance's row until the next attach;
//   - it parks a crash-looping instance (scaled to 0 and recorded stopped,
//     the reason in its warning), naming an out-of-memory loop as one;
//   - it serves a running instance's published ports (-p, appPort) on the
//     daemon host (published_ports.go) and closes them when it stops;
//   - it keeps InstanceRuntime.kube (pod reason, node, agent, last restart,
//     published ports) current, so a TUI that connects later sees it at once.
//
// A pass over every instance every kubePodsBackstop catches anything an event
// did not deliver; nothing here polls the cluster.

// kubePodsBackstop is the full pass's period; kubePodsPriorityCheck how
// often the supervisor's priorities are re-judged against whether a TUI
// watches.
const (
	kubePodsBackstop      = time.Minute
	kubePodsPriorityCheck = 2 * time.Second
	kubeParkTimeout       = time.Minute
	kubeParkRetry         = 30 * time.Second
)

// kubePodPool is the part of the pool kubePods drives (*kubernetes.Pool).
type kubePodPool interface {
	Track([]kubernetes.Tracked)
	Observe(clusterID, sts string) kubernetes.Observation
	OnChange(func(clusterID, sts string))
	Park(ctx context.Context, clusterID, sts string) error
	Forget(kubernetes.PodTarget)
	OnRunEnded(func(context.Context, kubernetes.RunEnd))
}

// kubePostRestart redoes a Fleet start's post-start work in an instance the
// cluster restarted on its own: the devcontainer's start hooks through its
// backend (backend.PostStarter), then fleet's own hook (the in-instance
// fleet binary and fleet.rc), then the start's shared-service work (the image
// mirror in the pod's docker engine, whose config a fresh pod lost). A
// backend that is not a PostStarter gets none of them.
func kubePostRestart(fleetName string, inst *fleet.Instance, homeDir string) {
	ps, ok := kubePostStarter(inst)
	if !ok {
		return
	}
	if err := ps.RunPostStart(inst.ContainerID); err != nil {
		// The backend's post-start (the devcontainer's start hooks through
		// the pod agent) failed: the pod is not reachable, so fleet's own
		// hook (fleet.rc, also over the agent) would only fail the same way.
		state.WriteWarn(fleetName, inst.Name, fmt.Sprintf("post-start after the cluster restarted the instance: %v", err))
		return
	}
	instanceops.RunPostStartHook(fleetName, inst, homeDir)
	kubeReviveSharedServices(fleetName, inst)
}

// kubePostStarter is inst's backend handle as a backend.PostStarter (false:
// it is none), and kubeReviveSharedServices instanceops.ReviveSharedServices;
// package vars so tests stand in their own.
var (
	kubePostStarter = func(inst *fleet.Instance) (backend.PostStarter, bool) {
		ps, ok := backendutil.NewForInstance(inst, false).(backend.PostStarter)
		return ps, ok
	}
	kubeReviveSharedServices = instanceops.ReviveSharedServices
)

// kubePublishFor is a running instance's published ports and the dial into
// it that serves them: its backend handle's (backend.PortPublisher and
// backend.PortDialer); false when the handle has none.
func kubePublishFor(inst *fleet.Instance) ([]backend.PublishedPort, portDial, bool) {
	b := backendutil.NewForInstance(inst, false)
	pub, ok := b.(backend.PortPublisher)
	dialer, ok2 := b.(backend.PortDialer)
	if !ok || !ok2 {
		return nil, nil, false
	}
	ports, err := pub.PublishedPorts(inst.ContainerID)
	if err != nil {
		return nil, nil, false
	}
	containerID := inst.ContainerID
	return ports, func(ctx context.Context, port int) (io.ReadWriteCloser, error) {
		return dialer.DialPort(ctx, containerID, port)
	}, true
}

// kubeAudioFor is why an instance can have no audio, as InstanceRuntime.kube
// carries it (kubernetes.AudioNoRoot); "" when it can, or its handle does not
// say.
func kubeAudioFor(inst *fleet.Instance) string {
	if a, ok := backendutil.NewForInstance(inst, false).(interface{ AudioReason() string }); ok {
		return a.AudioReason()
	}
	return ""
}

// kubeTargetFor is the pod the supervisor keeps an agent up in for a running
// instance: its backend handle's (kubernetes.TargetProvider), nil when the
// handle provides none.
func kubeTargetFor(inst *fleet.Instance) *kubernetes.PodTarget {
	tp, ok := backendutil.NewForInstance(inst, false).(kubernetes.TargetProvider)
	if !ok {
		return nil
	}
	t, err := tp.PodTarget()
	if err != nil {
		return nil
	}
	return &t
}

type kubeRef struct{ cluster, sts string }

// key names the instance among the published ports.
func (r kubeRef) key() string { return r.cluster + "/" + r.sts }

// kubeInst is kubePods' state for one instance.
type kubeInst struct {
	fleet, name string
	inst        fleet.Instance
	homeDir     string

	// seenRestart is the latest restart (Restart.ID) handled: its notice
	// set while the instance ran, or let go while fleet was starting or
	// stopping it. postStarted is the latest one whose post-start work ran
	// (or was let go), postStartBusy whether some is running now.
	seenRestart   string
	postStarted   string
	postStartBusy bool
	// notice is the "sessions reset" restart shown until the next attach.
	notice        *backend.Restart
	sessionsEnded bool
	// audio is why the instance can have no audio ("" when it can).
	audio string

	parking   bool
	parkAfter time.Time
}

type kubePods struct {
	svc  *service
	pool func() kubePodPool
	now  func() time.Time
	// Cadences (the consts; tests shorten them before run).
	backstop, priorityCheck, parkTimeout, parkRetry time.Duration
	// targetFor, audioFor and postRestart are kubeTargetFor, kubeAudioFor and
	// kubePostRestart (tests stand in for the backend).
	targetFor   func(*fleet.Instance) *kubernetes.PodTarget
	audioFor    func(*fleet.Instance) string
	postRestart func(fleetName string, inst *fleet.Instance, homeDir string)
	// logs saves a run's container output when the pool sees it end, and
	// readBack recovers the runs it could not from pod-init's rolling copy
	// (kubePods.readBackRolling; tests stand in for the backend).
	logs     *kubeLogs
	readBack func(ctx context.Context, fleetName string, inst *fleet.Instance)
	// ports serves the running instances' published ports, as publishFor
	// (kubePublishFor; tests stand in for the backend) gives them.
	ports      *portPublisher
	publishFor func(*fleet.Instance) ([]backend.PublishedPort, portDial, bool)
	// evac moves a fleet off a node the cluster empties
	// (kube_evacuate.go).
	evac *kubeEvacuations

	wake chan struct{}

	mu       sync.Mutex
	state    *state.State // newest state handed in, not synced yet
	synced   *state.State // the state of the last sync
	all      bool         // evaluate every instance
	pending  map[kubeRef]bool
	insts    map[kubeRef]*kubeInst
	byName   map[string]kubeRef
	tracking bool // the pool has (had) something to track
	wanted   bool // a TUI watched at the last sync
}

func newKubePods(svc *service) *kubePods {
	k := &kubePods{
		svc:           svc,
		pool:          func() kubePodPool { return kubernetes.DefaultPool() },
		now:           time.Now,
		backstop:      kubePodsBackstop,
		priorityCheck: kubePodsPriorityCheck,
		parkTimeout:   kubeParkTimeout,
		parkRetry:     kubeParkRetry,
		targetFor:     kubeTargetFor,
		audioFor:      kubeAudioFor,
		postRestart:   kubePostRestart,
		logs:          newKubeLogs(),
		ports:         newPortPublisher(),
		publishFor:    kubePublishFor,
		wake:          make(chan struct{}, 1),
		pending:       map[kubeRef]bool{},
		insts:         map[kubeRef]*kubeInst{},
		byName:        map[string]kubeRef{},
	}
	k.readBack = k.readBackRolling
	k.evac = newKubeEvacuations(k)
	return k
}

// readBackRolling saves inst's runs that pod-init's rolling copy holds and
// container.log does not (kubeLogs.readBackRolling), through its backend.
func (k *kubePods) readBackRolling(ctx context.Context, fleetName string, inst *fleet.Instance) {
	if r, ok := backendutil.NewForInstance(inst, false).(rollingReader); ok {
		k.logs.readBackRolling(ctx, fleetName, inst.Name, r)
	}
}

// started is told that Fleet started an instance: a Kubernetes one's runs
// that ended unsaved (its pod went before its log was read, or while fleetd
// was down) are recovered from the rolling copy, in the background.
func (k *kubePods) started(fleetName, instance string) {
	st, err := state.Load()
	if err != nil {
		return
	}
	f := st.Fleets[fleetName]
	if f == nil {
		return
	}
	inst, err := f.GetInstance(instance)
	if err != nil || !isKubeInstance(inst) {
		return
	}
	go k.readBack(context.Background(), fleetName, inst)
}

func (k *kubePods) signal() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// onState hands in a new state (the hub's state listener; it must not
// block).
func (k *kubePods) onState(st *state.State) {
	k.mu.Lock()
	k.state = st
	k.mu.Unlock()
	k.signal()
}

// poke marks an instance for evaluation (the pool's change hook).
func (k *kubePods) poke(clusterID, sts string) {
	k.mu.Lock()
	k.pending[kubeRef{clusterID, sts}] = true
	k.mu.Unlock()
	k.signal()
}

// pokeFleet marks every instance of the fleet on the connection for
// evaluation.
func (k *kubePods) pokeFleet(clusterID, fleetName string) {
	k.mu.Lock()
	for ref, ki := range k.insts {
		if ref.cluster == clusterID && ki.fleet == fleetName {
			k.pending[ref] = true
		}
	}
	k.mu.Unlock()
	k.signal()
}

// attached clears the instance's "sessions reset" notice: someone attached
// a terminal to it.
func (k *kubePods) attached(fleetName, instance string) {
	k.mu.Lock()
	ref, ok := k.byName[fleetName+"/"+instance]
	if ki := k.insts[ref]; !ok || ki == nil || ki.notice == nil {
		k.mu.Unlock()
		return
	}
	k.insts[ref].notice = nil
	k.pending[ref] = true
	k.mu.Unlock()
	k.signal()
}

// run processes state changes, pool events and the backstop until ctx ends.
// The serve loop starts it once fleetd serves.
func (k *kubePods) run(ctx context.Context) {
	backstop := time.NewTicker(k.backstop)
	defer backstop.Stop()
	prio := time.NewTicker(k.priorityCheck)
	defer prio.Stop()
	hooked := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.wake:
		case <-backstop.C:
			k.mu.Lock()
			tracking := k.tracking
			k.mu.Unlock()
			if !tracking {
				continue
			}
			// Also catches a state change the hub did not hand in.
			if st, err := state.Load(); err == nil {
				k.mu.Lock()
				k.state = st
				k.all = true
				k.mu.Unlock()
			}
		case <-prio.C:
			k.mu.Lock()
			if k.tracking && k.wanted != k.svc.hub.runtimeWanted.Load() && k.state == nil {
				// A TUI came or went: the same state, other priorities.
				k.state = k.synced
			}
			k.mu.Unlock()
		}
		if !hooked && k.hasWork() {
			k.pool().OnChange(k.poke)
			k.pool().OnRunEnded(k.logs.onRunEnded)
			hooked = true
		}
		k.process(ctx)
	}
}

// hasWork reports whether there is a Kubernetes instance (or was): until
// then the pool is never touched.
func (k *kubePods) hasWork() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tracking {
		return true
	}
	return k.state != nil && hasKubeInstance(k.state)
}

func hasKubeInstance(st *state.State) bool {
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if isKubeInstance(inst) {
				return true
			}
		}
	}
	return false
}

func isKubeInstance(inst *fleet.Instance) bool {
	return inst.Backend == fleet.BackendKubernetes && inst.KubeCluster != "" && names.ValidHash(inst.KubeKey)
}

func (k *kubePods) process(ctx context.Context) {
	k.mu.Lock()
	st := k.state
	k.state = nil
	tracking := k.tracking
	k.mu.Unlock()
	if st != nil && (tracking || hasKubeInstance(st)) {
		k.sync(st)
	}
	k.mu.Lock()
	var refs []kubeRef
	if k.all {
		for ref := range k.insts {
			refs = append(refs, ref)
		}
		k.all = false
		k.pending = map[kubeRef]bool{}
	} else {
		for ref := range k.pending {
			refs = append(refs, ref)
		}
		clear(k.pending)
	}
	k.mu.Unlock()
	for _, ref := range refs {
		k.evaluate(ctx, ref)
	}
}

// sync converges the pool on st: every Kubernetes record is watched, the
// running ones with a target supervised; a record that is gone is forgotten.
func (k *kubePods) sync(st *state.State) {
	interactive := k.svc.hub.runtimeWanted.Load()
	mic := false
	if cfg, err := state.LoadConfig(); err == nil {
		mic = cfg.MicSettings.Enabled
	}
	type entry struct {
		ref   kubeRef
		ki    *kubeInst
		inst  *fleet.Instance
		f     *fleet.Fleet
		audio string
	}
	var entries []entry
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if !isKubeInstance(inst) {
				continue
			}
			ref := kubeRef{inst.KubeCluster, names.Instance(fleetName, inst.Name, inst.KubeKey)}
			entries = append(entries, entry{ref: ref, inst: inst, f: f, audio: k.audioFor(inst)})
		}
	}
	items := make([]kubernetes.Tracked, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		item := kubernetes.Tracked{Cluster: e.ref.cluster, StatefulSet: e.ref.sts}
		if e.inst.Status == fleet.StatusRunning {
			if t := k.targetFor(e.inst); t != nil {
				t.Fleet, t.Instance = e.f.Name, e.inst.Name
				t.Priority = kubePriority(interactive, mic, e.f, e.inst)
				item.Target = t
			}
		}
		items = append(items, item)
	}

	var gone []kubeRef
	var unbound [][2]string // fleet, instance
	k.mu.Lock()
	seen := map[kubeRef]bool{}
	for _, e := range entries {
		seen[e.ref] = true
		ki := k.insts[e.ref]
		if ki == nil {
			ki = &kubeInst{}
			k.insts[e.ref] = ki
		}
		ki.fleet, ki.name, ki.inst, ki.homeDir, ki.audio = e.f.Name, e.inst.Name, *e.inst, e.f.Settings.HomeDir, e.audio
		k.byName[ki.fleet+"/"+ki.name] = e.ref
		k.pending[e.ref] = true
	}
	for ref, ki := range k.insts {
		if !seen[ref] {
			gone = append(gone, ref)
			delete(k.insts, ref)
			if k.byName[ki.fleet+"/"+ki.name] == ref {
				delete(k.byName, ki.fleet+"/"+ki.name)
				unbound = append(unbound, [2]string{ki.fleet, ki.name})
			}
		}
	}
	k.tracking = true
	k.wanted = interactive
	k.synced = st
	k.mu.Unlock()

	if len(unbound) > 0 {
		// No Kubernetes instance has the name any more: what the row said of
		// the old one's pod must not show on whatever takes the name next.
		ups := make([]runtimeUpdate, 0, len(unbound))
		for _, n := range unbound {
			ups = append(ups, runtimeUpdate{n[0], n[1], func(r *fleetgrpc.InstanceRuntime) {
				r.Kube, r.LiveStatus = nil, fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_UNSPECIFIED
			}})
		}
		k.svc.hub.post(func(h *hub) { h.applyRuntime(ups) })
	}
	pool := k.pool()
	for _, ref := range gone {
		// Destroyed (or its record replaced): nothing of its pod is kept.
		k.ports.drop(ref.key())
		pool.Forget(kubernetes.PodTarget{Cluster: ref.cluster, StatefulSet: ref.sts})
	}
	pool.Track(items)
}

// kubePriority orders an instance's agent bootstrap: interactive while a TUI
// watches; from fleetd's state otherwise (after a restart the live signals
// are gone): fleets with Fleet MCP on, automation runs, and every instance
// while the microphone is on; else background. (Audio flowing on a pod when
// its session dropped is the pool's own reason to reconnect it first.)
func kubePriority(interactive, mic bool, f *fleet.Fleet, inst *fleet.Instance) kubernetes.Priority {
	switch {
	case interactive:
		return kubernetes.PriorityInteractive
	case f.Settings.FleetMCP, inst.Automated, mic:
		return kubernetes.PriorityState
	}
	return kubernetes.PriorityBackground
}

// evaluate acts on what the pool observes of one instance.
func (k *kubePods) evaluate(ctx context.Context, ref kubeRef) {
	obs := k.pool().Observe(ref.cluster, ref.sts)
	now := k.now()
	k.mu.Lock()
	ki := k.insts[ref]
	if ki == nil {
		k.mu.Unlock()
		return
	}
	running := ki.inst.Status == fleet.StatusRunning
	if r := obs.Restart; r != nil && r.ID != ki.seenRestart {
		ki.seenRestart = r.ID
		if running {
			restart := *r
			ki.notice, ki.sessionsEnded = &restart, obs.SessionsEnded
			flog.Warn("kube: the cluster restarted an instance; its sessions ended", "fleet", ki.fleet, "instance", ki.name, "cause", string(r.Cause))
			if r.Cause == backend.RestartFleetMoved {
				// The move started it again as a Fleet start does, its
				// post-start work included.
				ki.postStarted = r.ID
			}
		} else {
			// Fleet itself is starting or stopping it: its own start runs
			// the post-start work.
			ki.postStarted = r.ID
		}
	}
	var postStart func()
	if running && obs.Live == backend.LiveStatusRunning && ki.seenRestart != ki.postStarted && !ki.postStartBusy {
		id, fleetName, inst, home := ki.seenRestart, ki.fleet, ki.inst, ki.homeDir
		ki.postStartBusy = true
		postStart = func() {
			// Recover any runs lost to a node failure first (the API-server
			// log of the old node is gone; pod-init's rolling copy on the
			// persisted disk survived), then re-run the post-start work.
			k.readBack(ctx, fleetName, &inst)
			k.postRestart(fleetName, &inst, home)
			k.mu.Lock()
			if cur := k.insts[ref]; cur != nil {
				cur.postStarted, cur.postStartBusy = id, false
				k.pending[ref] = true
			}
			k.mu.Unlock()
			k.signal()
		}
	}
	var park func()
	if loop := obs.CrashLoop; loop != nil && running && !ki.parking && !now.Before(ki.parkAfter) {
		ki.parking = true
		fleetName, name, key, l := ki.fleet, ki.name, ki.inst.KubeKey, *loop
		park = func() { k.park(ctx, ref, fleetName, name, key, l) }
	}
	kr := kubeRuntime(obs, ki)
	moving, blocked := k.evac.state(ref.cluster, ki.fleet)
	kr.NodeCordonedMoving = moving || (obs.MovingOff != "" && obs.Live != backend.LiveStatusRunning)
	kr.MoveBlocked = blocked
	fleetName, name := ki.fleet, ki.name
	inst := ki.inst
	k.mu.Unlock()

	if running {
		switch {
		case obs.Holder && obs.Disrupted != "":
			// Its pod holds the fleet's share on a node the cluster empties:
			// the fleet moves off it together.
			k.evac.want(ctx, ref.cluster, fleetName, obs.Node, obs.Disrupted)
		case obs.MovingOff != "" && (obs.Live != backend.LiveStatusRunning || obs.Node == obs.MovingOff):
			// A move a daemon began and did not finish (it stopped before
			// starting the fleet again).
			k.evac.want(ctx, ref.cluster, fleetName, obs.MovingOff, "the daemon restarted during a move")
		case obs.Holder && obs.Node != "":
			k.evac.settled(ref.cluster, fleetName)
		}
	}

	// A running instance's published ports listen on this host; a stopped
	// one's close.
	if running && obs.Live == backend.LiveStatusRunning {
		kr.PublishedPorts = k.publish(ref, fleetName, &inst)
	} else {
		k.ports.drop(ref.key())
	}
	live := liveStatusToProto(obs.Live)
	k.svc.hub.post(func(h *hub) {
		h.applyRuntime([]runtimeUpdate{{fleetName, name, func(r *fleetgrpc.InstanceRuntime) {
			r.Kube = kr
			r.LiveStatus = live
		}}})
	})
	if postStart != nil {
		go postStart()
	}
	if park != nil {
		go park()
	}
}

// publish serves inst's published ports and returns what each listener is.
func (k *kubePods) publish(ref kubeRef, fleetName string, inst *fleet.Instance) []*fleetgrpc.KubePublishedPort {
	ports, dial, ok := k.publishFor(inst)
	if !ok || len(ports) == 0 {
		k.ports.drop(ref.key())
		return nil
	}
	return k.ports.sync(ref.key(), publishedLabel(fleetName, inst.Name), inst.ContainerID, ports, dial)
}

// kubeRuntime is the instance's InstanceRuntime.kube. Called with k.mu held.
func kubeRuntime(obs kubernetes.Observation, ki *kubeInst) *fleetgrpc.KubeInstanceRuntime {
	kr := &fleetgrpc.KubeInstanceRuntime{PodReason: obs.Reason, Node: obs.Node, Agent: kubeAgentToProto(obs.Agent), AudioUnavailable: ki.audio}
	if n := ki.notice; n != nil {
		kr.RestartCause = string(n.Cause)
		kr.RestartHint = kubernetes.RestartHint(n.Cause)
		kr.RestartedAt = timestamppb.New(n.At)
		kr.SessionsEnded = ki.sessionsEnded
	}
	return kr
}

func kubeAgentToProto(a kubernetes.AgentState) fleetgrpc.KubeAgentState {
	switch a {
	case kubernetes.AgentConnected:
		return fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_CONNECTED
	case kubernetes.AgentReconnecting:
		return fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_RECONNECTING
	case kubernetes.AgentUnknown:
		return fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_UNKNOWN
	}
	return fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_UNSPECIFIED
}

// park scales a crash-looping instance to 0 and records it stopped, the
// loop's message (its code first) in its warning and as its error, which
// every client reads (the TUI's recovery dialog, MCP fleet_list's error) and
// the next start clears. A failed park is tried again after parkRetry.
func (k *kubePods) park(ctx context.Context, ref kubeRef, fleetName, name, key string, loop kubernetes.CrashLoop) {
	pctx, cancel := context.WithTimeout(ctx, k.parkTimeout)
	err := k.pool().Park(pctx, ref.cluster, ref.sts)
	cancel()
	parked := false
	if err == nil {
		err = state.Update(func(st *state.State) error {
			f, ok := st.Fleets[fleetName]
			if !ok {
				return nil
			}
			inst, ferr := f.GetInstance(name)
			if ferr != nil || inst.KubeKey != key || inst.Status != fleet.StatusRunning {
				// Gone, replaced, or moved on (a stop, a rebuild) meanwhile.
				return nil
			}
			inst.Status = fleet.StatusStopped
			inst.Error = loop.Message()
			parked = true
			return nil
		})
	}
	k.mu.Lock()
	if ki := k.insts[ref]; ki != nil {
		ki.parking = false
		if err != nil {
			ki.parkAfter = k.now().Add(k.parkRetry)
		} else if parked {
			ki.inst.Status = fleet.StatusStopped
		}
	}
	k.mu.Unlock()
	switch {
	case err != nil:
		flog.Error("kube: parking a crash-looping instance failed; trying again", "fleet", fleetName, "instance", name, "code", loop.Code(), "err", err)
	case parked:
		state.WriteWarn(fleetName, name, loop.Message())
		flog.Warn("kube: parked a crash-looping instance as stopped", "fleet", fleetName, "instance", name, "code", loop.Code(), "oom", loop.OOM, "restarts", loop.Restarts)
		loadAndPush(k.svc.hub)
	}
}
