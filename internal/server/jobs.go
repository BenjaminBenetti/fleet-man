package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// jobs.go implements the server-side lifecycle jobs (create/clone/destroy/
// start/stop). The server owns them so they survive client death and so EVERY
// state write goes through the single fleet process (the cross-process half of
// the issue #63 fix; the in-process half is state.Update).
//
// A job runs in a server-owned goroutine and emits a JobEvent sequence whose
// first event is always JobStarted and whose last is always JobDone (the
// contract in jobs.proto). The calling RPC relays those events to its stream but
// does NOT own the job: if the client disconnects the goroutine keeps running,
// and a JobSummary stays on GetState so a watcher can learn the job exists.
//
// Progress granularity is intentionally COARSE here (JobStarted -> work ->
// JobDone): per the proto's terminal-truth note the authoritative terminal state
// is the StateChanged snapshot, and the job stream is best-effort progress UI.
// Fine-grained JobStep progress can be threaded through create.Run later.

// --- work seams (overridable in tests so the engine is exercised without docker) ---

var jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, backendType fleet.BackendType) error {
	return create.Run(fleetName, instanceName, remote, branch, verbose, backendType)
}

var jobRunClone = func(fleetName, srcInstance, destInstance string, verbose bool) error {
	return create.RunClone(fleetName, srcInstance, destInstance, verbose)
}

var jobRunStart = func(fleetName, instanceName string) error {
	_, err := instanceops.StartInstance(fleetName, instanceName)
	return err
}

var jobRunStop = func(fleetName, instanceName string) error {
	_, err := instanceops.StopInstance(fleetName, instanceName)
	return err
}

var jobRunRebuild = func(fleetName, instanceName string) error {
	return create.RunRebuild(fleetName, instanceName, false)
}

// stopBuildkitServer is the buildkit teardown seam (a package var so the destroy
// paths can be exercised in tests without docker).
var stopBuildkitServer = buildkit.StopSharedServer

// teardownFleetBuildkit removes a fleet's shared buildkit CONTAINER when the
// fleet had the feature enabled, so it doesn't orphan or (given its
// restart=unless-stopped policy) auto-restart after the fleet is gone. It
// deliberately LEAVES the .buildkit cache directory on disk: a shared build
// cache is the whole point of the feature, so it should persist across a fleet
// teardown and warm the next instance created for a fleet of the same name —
// exactly like the persisted .claude/.codex/.config/gh mount dirs already do.
// Best-effort: a failure becomes a warning, never an abort. Shared by the
// destroy job and the DestroyFleet RPC so the container is reclaimed no matter
// which delete path runs (destroy_fleet=true vs. removing an already-empty fleet).
func teardownFleetBuildkit(fleetName string, enabled bool) []string {
	if !enabled {
		return nil
	}
	if err := stopBuildkitServer(fleetName); err != nil {
		return []string{fmt.Sprintf("stop buildkit server: %v", err)}
	}
	return nil
}

// stopDebCacheServer / stopImageCacheServer / removeFleetNetwork are the deb/
// image cache + network teardown seams (package vars so the destroy paths can be
// exercised in tests without docker). Mirror stopBuildkitServer.
var stopDebCacheServer = debcache.StopSharedServer
var stopImageCacheServer = imagecache.StopSharedServer
var removeFleetNetwork = fleetnet.RemoveNetwork

// teardownFleetDebCache removes a fleet's shared deb cache CONTAINER when the
// fleet had the feature enabled. Like teardownFleetBuildkit it LEAVES the
// .aptcache directory on disk so the cache warms the next fleet of the same
// name. Best-effort: a failure becomes a warning, never an abort.
func teardownFleetDebCache(fleetName string, enabled bool) []string {
	if !enabled {
		return nil
	}
	if err := stopDebCacheServer(fleetName); err != nil {
		return []string{fmt.Sprintf("stop deb cache server: %v", err)}
	}
	return nil
}

// teardownFleetImageCache removes a fleet's shared image cache CONTAINER when
// the fleet had the feature enabled (leaving .imgcache on disk). Best-effort.
func teardownFleetImageCache(fleetName string, enabled bool) []string {
	if !enabled {
		return nil
	}
	if err := stopImageCacheServer(fleetName); err != nil {
		return []string{fmt.Sprintf("stop image cache server: %v", err)}
	}
	return nil
}

// teardownFleetNetwork removes a fleet's shared cache network when the fleet
// used a network-based cache (deb or image). It must run AFTER the cache
// containers and instances are gone, since docker refuses to remove a network
// with active endpoints. Best-effort.
func teardownFleetNetwork(fleetName string, enabled bool) []string {
	if !enabled {
		return nil
	}
	if err := removeFleetNetwork(fleetName); err != nil {
		return []string{fmt.Sprintf("remove fleet network: %v", err)}
	}
	return nil
}

// jobDownInstance tears down one provisioned instance's container. Best-effort:
// a backend failure is returned so the caller can WARN, but teardown proceeds.
var jobDownInstance = func(inst *fleet.Instance) error {
	if inst.ContainerID == "" {
		return nil
	}
	var b backend.Backend
	if inst.Backend == "" {
		b = backendutil.New(fleet.BackendDevcontainer, false)
	} else {
		b = backendutil.NewForInstance(inst, false)
	}
	return b.Down(inst.ContainerID)
}

// --- job registry ---

// finishedJobRetention bounds the finished-job registry (FIFO eviction). It
// only needs to cover a reasonable polling window for async callers; a poller
// that comes back later falls back to the instance record (fleet_list), which
// holds the durable status/error.
const finishedJobRetention = 256

type jobManager struct {
	seq    int64
	mu     sync.Mutex
	active map[string]*job
	// finished retains terminal jobs (bounded by finishedJobRetention) so an
	// async caller — e.g. the MCP fleet_job_status tool — can still read a job's
	// outcome after it completes. finishedIDs is the FIFO eviction order.
	finished    map[string]*job
	finishedIDs []string
}

func newJobManager() *jobManager {
	return &jobManager{active: make(map[string]*job), finished: make(map[string]*job)}
}

type job struct {
	summary *fleetgrpc.JobSummary

	mu      sync.Mutex
	history []*fleetgrpc.JobEvent
	subs    map[chan *fleetgrpc.JobEvent]struct{}
	done    bool
}

func (m *jobManager) start(kind fleetgrpc.JobKind, fleetName, instanceName string, startedAt time.Time) *job {
	id := fmt.Sprintf("job-%d-%d", os.Getpid(), atomic.AddInt64(&m.seq, 1))
	j := &job{
		summary: &fleetgrpc.JobSummary{
			JobId:     id,
			Kind:      kind,
			Fleet:     fleetName,
			Instance:  instanceName,
			StartedAt: timestamppb.New(startedAt),
		},
		subs: make(map[chan *fleetgrpc.JobEvent]struct{}),
	}
	m.mu.Lock()
	m.active[id] = j
	m.mu.Unlock()
	return j
}

func (m *jobManager) finish(id string) {
	m.mu.Lock()
	j := m.active[id]
	delete(m.active, id)
	if j != nil {
		m.finished[id] = j
		m.finishedIDs = append(m.finishedIDs, id)
		for len(m.finishedIDs) > finishedJobRetention {
			delete(m.finished, m.finishedIDs[0])
			m.finishedIDs = m.finishedIDs[1:]
		}
	}
	m.mu.Unlock()
	if j != nil {
		j.closeSubs()
	}
}

// get returns a job by id — in-flight or recently finished — or nil if unknown
// (never started, evicted, or pre-dating a daemon restart).
func (m *jobManager) get(id string) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.active[id]; ok {
		return j
	}
	return m.finished[id]
}

// summaries snapshots the in-flight jobs for GetState/Watch.
func (m *jobManager) summaries() []*fleetgrpc.JobSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*fleetgrpc.JobSummary, 0, len(m.active))
	for _, j := range m.active {
		out = append(out, j.summary)
	}
	return out
}

// emit appends an event to the job history and fans it out to live subscribers.
// The non-blocking send under the lock can't race a closeSubs (also locked), so
// it never sends on a closed channel; a full buffer drops (coarse jobs — a few
// events — never fill the 64-slot buffer, and a re-subscribe always replays the
// full history).
func (j *job) emit(ev *fleetgrpc.JobEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.history = append(j.history, ev)
	if ev.GetDone() != nil {
		j.done = true
	}
	if p := ev.GetProgress(); p != nil {
		j.summary.CurrentStep = p.GetStep()
	}
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe returns the event history so far plus a live channel for future
// events. If the job is already terminal the channel is nil (history holds the
// JobDone, nothing more is coming).
func (j *job) subscribe() ([]*fleetgrpc.JobEvent, chan *fleetgrpc.JobEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	hist := append([]*fleetgrpc.JobEvent(nil), j.history...)
	if j.done {
		return hist, nil
	}
	ch := make(chan *fleetgrpc.JobEvent, 64)
	j.subs[ch] = struct{}{}
	return hist, ch
}

func (j *job) unsubscribe(ch chan *fleetgrpc.JobEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.subs, ch)
}

func (j *job) closeSubs() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for ch := range j.subs {
		close(ch)
		delete(j.subs, ch)
	}
}

// outcome returns the job's terminal JobDone, or nil while it is still running.
func (j *job) outcome() *fleetgrpc.JobDone {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.done {
		return nil
	}
	for i := len(j.history) - 1; i >= 0; i-- {
		if d := j.history[i].GetDone(); d != nil {
			return d
		}
	}
	return nil
}

// runJob is the shared driver: emit JobStarted, run work (which may emit further
// events via emit), emit JobDone, then unregister + close subscribers. It runs
// in its own goroutine so it is not tied to the calling RPC's lifetime.
//
// Every job is also recorded in the event log (started, each warning, then
// finished/failed) so fleet.log carries an operation-level trail for ALL
// lifecycle jobs — including those whose work has no logging of its own.
func (s *service) runJob(j *job, work func() (finalInstance *fleetgrpc.Instance, warnings []string, err error)) {
	start := time.Now()
	kind := jobKindName(j.summary.Kind)
	flog.Info("job started", "job", j.summary.JobId, "kind", kind, "fleet", j.summary.Fleet, "instance", j.summary.Instance)
	j.emit(&fleetgrpc.JobEvent{JobId: j.summary.JobId, Event: &fleetgrpc.JobEvent_Started{Started: &fleetgrpc.JobStarted{
		JobId:     j.summary.JobId,
		Kind:      j.summary.Kind,
		Fleet:     j.summary.Fleet,
		Instance:  j.summary.Instance,
		StartedAt: j.summary.StartedAt,
	}}})

	finalInstance, warnings, err := work()

	for _, w := range warnings {
		flog.Warn("job warning", "job", j.summary.JobId, "kind", kind, "fleet", j.summary.Fleet, "instance", j.summary.Instance, "warn", w)
	}
	if err != nil {
		flog.Error("job failed", "job", j.summary.JobId, "kind", kind, "fleet", j.summary.Fleet, "instance", j.summary.Instance, "ms", flog.MillisSince(start), "err", err)
	} else {
		flog.Info("job finished", "job", j.summary.JobId, "kind", kind, "fleet", j.summary.Fleet, "instance", j.summary.Instance, "ms", flog.MillisSince(start))
	}

	done := &fleetgrpc.JobDone{
		Success:  err == nil,
		Instance: finalInstance,
		Ms:       time.Since(start).Milliseconds(),
		Warnings: warnings,
	}
	if err != nil {
		msg := err.Error()
		done.Error = &msg
	}
	j.emit(&fleetgrpc.JobEvent{JobId: j.summary.JobId, Event: &fleetgrpc.JobEvent_Done{Done: done}})
	s.jobs.finish(j.summary.JobId)
	// Nudge a fresh snapshot to Watch subscribers (the work already persisted via
	// state.Update; this just avoids waiting for the next poller tick).
	s.pushState()
}

// jobKindName renders a JobKind as a compact event-log value: "create_instance"
// rather than the proto's "JOB_KIND_CREATE_INSTANCE".
func jobKindName(k fleetgrpc.JobKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "JOB_KIND_"))
}

// relay streams a job's events (history then live) to a gRPC server stream until
// the job's JobDone is sent or the client disconnects. The job keeps running
// regardless — relay only governs THIS client's view.
func relay(j *job, stream interface {
	Send(*fleetgrpc.JobEvent) error
	Context() context.Context
}) error {
	hist, ch := j.subscribe()
	for _, ev := range hist {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	if ch == nil {
		return nil
	}
	defer j.unsubscribe(ch)
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			if ev.GetDone() != nil {
				return nil
			}
		}
	}
}

// pushState reloads the persisted state and broadcasts it to the hub. Used after
// a job mutates state so Watch subscribers update promptly.
func (s *service) pushState() {
	st, err := state.Load()
	if err != nil {
		return
	}
	snapshot := protoconv.StateToProto(st)
	s.hub.post(func(h *hub) { h.setState(snapshot) })
}

// loadInstanceSnapshot reads the current persisted record for an instance and
// returns it as proto (for JobDone.instance). Returns nil if absent.
func loadInstanceSnapshot(fleetName, instanceName string) *fleetgrpc.Instance {
	st, err := state.Load()
	if err != nil {
		return nil
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		return nil
	}
	return protoconv.InstanceToProto(inst)
}

// --- RPC handlers ---

// Each lifecycle RPC below is split into a start*Job half (validate, pre-write
// the transitional record, start the server-owned job goroutine) and a thin
// streaming wrapper that relays the job's events. The split lets the MCP tools
// start a job and return its handle immediately (async-first, issue #134)
// while the gRPC path keeps streaming until JobDone.

// startCreateInstanceJob pre-creates the StatusCreating record server-side
// (this removes the client-side pre-create write that drove issue #63), then
// starts the provisioning job.
func (s *service) startCreateInstanceJob(req *fleetgrpc.CreateInstanceRequest, automated bool) (*job, error) {
	fleetName, instanceName := req.GetFleet(), req.GetInstance()
	if fleetName == "" || instanceName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet and instance are required")
	}
	if err := fleet.ValidateInstanceName(instanceName); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	backendType := s.resolveBackend(req.GetBackend())
	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, instanceName, fleetName)

	var remote string
	err := state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			if req.Remote == nil || req.GetRemote() == "" {
				return status.Errorf(codes.NotFound, "fleet %q not found and no remote provided", fleetName)
			}
			// New fleets only (an existing record keeps whatever name it has):
			// the name becomes a workspace path component, so `..` or a slash
			// would escape ~/.fleet/workspaces before anything is cloned/copied.
			if err := fleet.ValidateFleetName(fleetName); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			f = st.GetOrCreateFleet(fleetName, req.GetRemote())
		}
		if req.Remote != nil && req.GetRemote() != "" {
			remote = req.GetRemote()
			// A per-instance --repo may point at another git URL, but not
			// change KIND: a template copy inside a git fleet (or a clone
			// inside a template fleet) would leave the record describing
			// something its instances are not.
			if ok && fleet.IsTemplateRemote(remote) != fleet.IsTemplateRemote(f.Remote) {
				return status.Errorf(codes.InvalidArgument,
					"remote %q does not match fleet %q (%s): a fleet is either a git remote or a %s template, not both",
					remote, fleetName, remoteKind(f.Remote), fleet.TemplateScheme)
			}
		} else {
			remote = f.Remote
		}
		// An UNSPECIFIED backend resolved to the host default (which may be
		// coder) — for a template fleet, devcontainer is the only backend that
		// can consume a copied directory, so narrow it the way the TUI's add-
		// instance dialog does. An explicit coder/codespaces still fails fast.
		if req.GetBackend() == fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED && fleet.IsTemplateRemote(remote) {
			backendType = fleet.BackendDevcontainer
		}
		if err := validateTemplateRemote(remote, req.GetBranch(), backendType, wsDir); err != nil {
			return err
		}
		if _, err := f.GetInstance(instanceName); err == nil {
			return status.Errorf(codes.AlreadyExists, "instance %s/%s already exists", fleetName, instanceName)
		}
		return f.AddInstance(&fleet.Instance{
			Name:         instanceName,
			DisplayName:  instanceName,
			Config:       ".devcontainer/devcontainer.json",
			WorkspaceDir: wsDir,
			CreatedAt:    time.Now(),
			Status:       fleet.StatusCreating,
			Backend:      backendType,
			Branch:       req.GetBranch(),
			Automated:    automated,
		})
	})
	if err != nil {
		return nil, err
	}
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, fleetName, instanceName, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunCreate(fleetName, instanceName, remote, req.GetBranch(), req.GetVerbose(), backendType)
		return loadInstanceSnapshot(fleetName, instanceName), nil, err
	})
	return j, nil
}

// remoteKind names a fleet remote's kind for error messages.
func remoteKind(remote string) string {
	if fleet.IsTemplateRemote(remote) {
		return "template " + remote
	}
	return "git " + remote
}

// validateTemplateRemote fails a create FAST — before the StatusCreating record
// exists — when the fleet's remote is a file:// template and the request asks
// for something a copied directory cannot honor (a branch, a non-devcontainer
// backend), or when the template directory is missing on THIS host. create.Run
// re-checks the same rules; doing it here turns a would-be failed instance
// record into an immediate RPC error for `fleet up` / the TUI / MCP.
func validateTemplateRemote(remote, branch string, backendType fleet.BackendType, wsDir string) error {
	if !fleet.IsTemplateRemote(remote) {
		return nil
	}
	if err := fleet.ValidateTemplateCreate(remote, branch, backendType); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	// ValidateTemplateCreate already parsed the URL, so a failure here is the
	// directory itself (missing / not a dir) — a precondition on THIS host.
	dir, err := fleet.ResolveTemplateDir(remote)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	// A template that contains its own workspace dir can never be copied.
	if err := fleet.ValidateTemplateWorkspace(dir, wsDir); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// CreateInstance starts the provisioning job and relays its events.
func (s *service) CreateInstance(req *fleetgrpc.CreateInstanceRequest, stream fleetgrpc.FleetService_CreateInstanceServer) error {
	j, err := s.startCreateInstanceJob(req, false)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

// startCloneInstanceJob pre-creates the StatusCloning destination record
// (copying the source's Config/Backend/Tag/Color/Branch per the contract), then
// starts the clone job.
func (s *service) startCloneInstanceJob(req *fleetgrpc.CloneInstanceRequest) (*job, error) {
	fleetName := req.GetFleet()
	srcName, destName := req.GetSourceInstance(), req.GetNewInstance()
	if fleetName == "" || srcName == "" || destName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet, source_instance and new_instance are required")
	}
	if err := fleet.ValidateInstanceName(destName); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetNewDisplayName() != "" {
		if err := fleet.ValidateInstanceName(req.GetNewDisplayName()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, destName, fleetName)
	err := state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
		}
		src, err := f.GetInstance(srcName)
		if err != nil {
			return status.Errorf(codes.NotFound, "source instance %s/%s not found", fleetName, srcName)
		}
		if _, err := f.GetInstance(destName); err == nil {
			return status.Errorf(codes.AlreadyExists, "instance %s/%s already exists", fleetName, destName)
		}
		display := destName
		if req.NewDisplayName != nil && req.GetNewDisplayName() != "" {
			display = req.GetNewDisplayName()
		}
		dest := &fleet.Instance{
			Name:         destName,
			DisplayName:  display,
			Config:       src.Config,
			WorkspaceDir: wsDir,
			CreatedAt:    time.Now(),
			Status:       fleet.StatusCloning,
			Backend:      src.Backend,
			Tag:          src.Tag,
			Color:        src.Color,
			Branch:       src.Branch,
		}
		if req.TagOverride != nil {
			dest.Tag = req.GetTagOverride()
		}
		if req.ColorOverride != nil {
			dest.Color = req.GetColorOverride()
		}
		if req.BranchOverride != nil {
			dest.Branch = req.GetBranchOverride()
		}
		return f.AddInstance(dest)
	})
	if err != nil {
		return nil, err
	}
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_CLONE_INSTANCE, fleetName, destName, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunClone(fleetName, srcName, destName, false)
		return loadInstanceSnapshot(fleetName, destName), nil, err
	})
	return j, nil
}

// CloneInstance starts the clone job and relays its events.
func (s *service) CloneInstance(req *fleetgrpc.CloneInstanceRequest, stream fleetgrpc.FleetService_CloneInstanceServer) error {
	j, err := s.startCloneInstanceJob(req)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

func (s *service) startStartInstanceJob(req *fleetgrpc.StartInstanceRequest) (*job, error) {
	if req.GetFleet() == "" || req.GetInstance() == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet and instance are required")
	}
	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_START_INSTANCE, req.GetFleet(), req.GetInstance(), time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunStart(req.GetFleet(), req.GetInstance())
		return loadInstanceSnapshot(req.GetFleet(), req.GetInstance()), nil, err
	})
	return j, nil
}

func (s *service) StartInstance(req *fleetgrpc.StartInstanceRequest, stream fleetgrpc.FleetService_StartInstanceServer) error {
	j, err := s.startStartInstanceJob(req)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

func (s *service) startStopInstanceJob(req *fleetgrpc.StopInstanceRequest) (*job, error) {
	if req.GetFleet() == "" || req.GetInstance() == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet and instance are required")
	}
	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_STOP_INSTANCE, req.GetFleet(), req.GetInstance(), time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunStop(req.GetFleet(), req.GetInstance())
		return loadInstanceSnapshot(req.GetFleet(), req.GetInstance()), nil, err
	})
	return j, nil
}

func (s *service) StopInstance(req *fleetgrpc.StopInstanceRequest, stream fleetgrpc.FleetService_StopInstanceServer) error {
	j, err := s.startStopInstanceJob(req)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

// startRebuildInstanceJob validates the target, refuses fast when the backend
// has no rebuild primitive or the instance is mid-transition, marks it
// StatusRebuilding, and starts the in-place reprovision job. The record is kept
// (unlike destroy) and flips back to running/failed on completion.
func (s *service) startRebuildInstanceJob(req *fleetgrpc.RebuildInstanceRequest) (*job, error) {
	fleetName, instanceName := req.GetFleet(), req.GetInstance()
	if fleetName == "" || instanceName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet and instance are required")
	}

	// Validate the target AND mark it StatusRebuilding in a single state.Update
	// so the check and the transitional-status pre-write are atomic: a second
	// concurrent rebuild (or a stop/destroy) can't slip past the guard in the
	// window between a separate read and write. This mirrors how
	// startCreateInstanceJob / startCloneInstanceJob validate-then-mark under one
	// lock. A typo / unsupported backend / mid-flight instance fails fast with a
	// clear gRPC error (state.Update propagates the closure's error) rather than
	// a failed job. The pre-write is the in-place sibling of the StatusCreating
	// pre-write, so pollers — fleet_list, async MCP callers, the TUI — see the
	// rebuild in flight instead of a stale running status.
	err := state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
		}
		inst, err := f.GetInstance(instanceName)
		if err != nil {
			return status.Errorf(codes.NotFound, "instance %q not found in fleet %q", instanceName, fleetName)
		}
		if isTransitionalStatus(inst.Status) {
			return status.Errorf(codes.FailedPrecondition, "instance %s/%s is %s; wait for it to settle before rebuilding", fleetName, instanceName, inst.Status)
		}
		// SupportsRebuild is a pure capability check, so construct by type
		// (no RegisterName / SSH-config file I/O under the state lock).
		if !backendutil.New(inst.Backend, false).SupportsRebuild() {
			return status.Errorf(codes.FailedPrecondition, "backend %q does not support rebuild", inst.Backend)
		}
		inst.Status = fleet.StatusRebuilding
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE, fleetName, instanceName, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunRebuild(fleetName, instanceName)
		return loadInstanceSnapshot(fleetName, instanceName), nil, err
	})
	return j, nil
}

// RebuildInstance starts the in-place rebuild job and relays its events.
func (s *service) RebuildInstance(req *fleetgrpc.RebuildInstanceRequest, stream fleetgrpc.FleetService_RebuildInstanceServer) error {
	j, err := s.startRebuildInstanceJob(req)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

// isTransitionalStatus reports whether a status reflects an in-flight lifecycle
// job, so a second mutating job (e.g. rebuild) can refuse to pile on. Mirrors
// the TUI's isTransitional check, kept server-side so CLI/MCP get the same
// guard.
func isTransitionalStatus(s fleet.InstanceStatus) bool {
	switch s {
	case fleet.StatusCreating, fleet.StatusCloning, fleet.StatusStopping,
		fleet.StatusStarting, fleet.StatusDeleting, fleet.StatusRebuilding:
		return true
	}
	return false
}

// startDestroyInstanceJob validates the target, marks it deleting, and starts
// the teardown job for one instance or (destroy_fleet) the whole fleet.
// Best-effort: container/workspace failures become warnings, and the record is
// removed regardless.
func (s *service) startDestroyInstanceJob(req *fleetgrpc.DestroyInstanceRequest) (*job, error) {
	fleetName := req.GetFleet()
	if fleetName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet is required")
	}
	if !req.GetDestroyFleet() && req.GetInstance() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance is required unless destroy_fleet is set")
	}

	// Validate the target exists so a typo'd / stale name fails fast (the CLI
	// surfaces this as a non-zero exit) rather than a silent best-effort no-op.
	if st, err := state.Load(); err == nil {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
		}
		if !req.GetDestroyFleet() {
			if _, err := f.GetInstance(req.GetInstance()); err != nil {
				return nil, status.Errorf(codes.NotFound, "instance %q not found in fleet %q", req.GetInstance(), fleetName)
			}
		}
	}

	target := req.GetInstance()
	// Mark the targets StatusDeleting server-side (the teardown sibling of the
	// StatusCreating pre-write) so pollers — fleet_list, async MCP callers, the
	// TUI — see the teardown in flight instead of a stale running/stopped
	// status. Best-effort: the job removes the records regardless, so a failed
	// mark only costs the transitional status.
	_ = state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return nil
		}
		for _, inst := range f.Instances {
			if req.GetDestroyFleet() || inst.Name == target {
				inst.Status = fleet.StatusDeleting
			}
		}
		return nil
	})
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_DESTROY_INSTANCE, fleetName, target, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		return nil, s.destroy(fleetName, target, req.GetDestroyFleet()), nil
	})
	return j, nil
}

// DestroyInstance starts the teardown job and relays its events.
func (s *service) DestroyInstance(req *fleetgrpc.DestroyInstanceRequest, stream fleetgrpc.FleetService_DestroyInstanceServer) error {
	j, err := s.startDestroyInstanceJob(req)
	if err != nil {
		return err
	}
	return relay(j, stream)
}

// destroy performs the teardown and returns the accumulated non-fatal warnings.
// The record removal always happens (best-effort) per the contract.
func (s *service) destroy(fleetName, instanceName string, destroyFleet bool) []string {
	var warnings []string

	// Snapshot the targets (container id + workspace) before mutating the record.
	type target struct {
		name, workspaceDir string
		inst               *fleet.Instance
	}
	var targets []target
	// buildkitEnabled is read from the live record before mutation so a
	// destroy_fleet can tear down the fleet's shared buildkit server after its
	// instances are down. Only meaningful when destroyFleet is set. The deb/image
	// cache + network teardown does NOT gate on the setting (see below).
	var buildkitEnabled bool
	if st, err := state.Load(); err == nil {
		if f, ok := st.Fleets[fleetName]; ok {
			buildkitEnabled = f.Settings.BuildkitServer
			for _, inst := range f.Instances {
				if destroyFleet || inst.Name == instanceName {
					targets = append(targets, target{name: inst.Name, workspaceDir: inst.WorkspaceDir, inst: inst})
				}
			}
		}
	}

	for _, t := range targets {
		if err := jobDownInstance(t.inst); err != nil {
			warnings = append(warnings, fmt.Sprintf("teardown %s/%s container: %v", fleetName, t.name, err))
		}
		if t.workspaceDir != "" {
			if err := os.RemoveAll(t.workspaceDir); err != nil {
				warnings = append(warnings, fmt.Sprintf("remove workspace %s: %v", t.workspaceDir, err))
			}
		}
	}

	// Fleet-level teardown: once every instance is down, remove the fleet's
	// shared cache containers (and the shared network). Single-instance destroys
	// leave them up — the fleet's other instances may still use them. Cache
	// directories are intentionally kept on disk (see each teardown helper). The
	// network is removed LAST, after the cache containers, so docker doesn't
	// refuse it for active endpoints.
	//
	// The deb/image cache + network teardown is UNCONDITIONAL on a full destroy
	// (not gated on the current setting): toggling a cache off does not stop its
	// running container, so gating on the live setting would orphan a container
	// the user disabled before destroying. The teardown helpers are idempotent
	// (a missing container/network is a no-op), so this is safe even for fleets
	// that never used a cache.
	warnings = append(warnings, teardownFleetBuildkit(fleetName, destroyFleet && buildkitEnabled)...)
	warnings = append(warnings, teardownFleetDebCache(fleetName, destroyFleet)...)
	warnings = append(warnings, teardownFleetImageCache(fleetName, destroyFleet)...)
	warnings = append(warnings, teardownFleetNetwork(fleetName, destroyFleet)...)

	_ = state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return nil
		}
		if destroyFleet {
			delete(st.Fleets, fleetName)
			return nil
		}
		_ = f.RemoveInstance(instanceName)
		return nil
	})
	s.pushState()
	return warnings
}

// resolveBackend turns the request's BackendType into a concrete backend: an
// explicit value wins; UNSPECIFIED falls back to config.json's DefaultBackend
// (when valid) and finally devcontainer.
func (s *service) resolveBackend(b fleetgrpc.BackendType) fleet.BackendType {
	switch b {
	case fleetgrpc.BackendType_BACKEND_TYPE_CODER:
		return fleet.BackendCoder
	case fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES:
		return fleet.BackendCodespaces
	case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
		return fleet.BackendDevcontainer
	default:
		if cfg, err := state.LoadConfig(); err == nil {
			if bt := fleet.BackendType(cfg.DefaultBackend); fleet.ValidateBackendType(bt) == nil {
				return bt
			}
		}
		return fleet.BackendDevcontainer
	}
}
