package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
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

type jobManager struct {
	seq    int64
	mu     sync.Mutex
	active map[string]*job
}

func newJobManager() *jobManager {
	return &jobManager{active: make(map[string]*job)}
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
	m.mu.Unlock()
	if j != nil {
		j.closeSubs()
	}
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

// runJob is the shared driver: emit JobStarted, run work (which may emit further
// events via emit), emit JobDone, then unregister + close subscribers. It runs
// in its own goroutine so it is not tied to the calling RPC's lifetime.
func (s *service) runJob(j *job, work func() (finalInstance *fleetgrpc.Instance, warnings []string, err error)) {
	start := time.Now()
	j.emit(&fleetgrpc.JobEvent{JobId: j.summary.JobId, Event: &fleetgrpc.JobEvent_Started{Started: &fleetgrpc.JobStarted{
		JobId:     j.summary.JobId,
		Kind:      j.summary.Kind,
		Fleet:     j.summary.Fleet,
		Instance:  j.summary.Instance,
		StartedAt: j.summary.StartedAt,
	}}})

	finalInstance, warnings, err := work()

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
	snapshot := stateToProto(st)
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
	return instanceToProto(inst)
}

// --- RPC handlers ---

// CreateInstance pre-creates the StatusCreating record server-side (this removes
// the client-side pre-create write that drove issue #63), then runs the
// provisioning job.
func (s *service) CreateInstance(req *fleetgrpc.CreateInstanceRequest, stream fleetgrpc.FleetService_CreateInstanceServer) error {
	fleetName, instanceName := req.GetFleet(), req.GetInstance()
	if fleetName == "" || instanceName == "" {
		return status.Error(codes.InvalidArgument, "fleet and instance are required")
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
			f = st.GetOrCreateFleet(fleetName, req.GetRemote())
		}
		if req.Remote != nil && req.GetRemote() != "" {
			remote = req.GetRemote()
		} else {
			remote = f.Remote
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
		})
	})
	if err != nil {
		return err
	}
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, fleetName, instanceName, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunCreate(fleetName, instanceName, remote, req.GetBranch(), req.GetVerbose(), backendType)
		return loadInstanceSnapshot(fleetName, instanceName), nil, err
	})
	return relay(j, stream)
}

// CloneInstance pre-creates the StatusCloning destination record (copying the
// source's Config/Backend/Tag/Color/Branch per the contract), then runs the
// clone job.
func (s *service) CloneInstance(req *fleetgrpc.CloneInstanceRequest, stream fleetgrpc.FleetService_CloneInstanceServer) error {
	fleetName := req.GetFleet()
	srcName, destName := req.GetSourceInstance(), req.GetNewInstance()
	if fleetName == "" || srcName == "" || destName == "" {
		return status.Error(codes.InvalidArgument, "fleet, source_instance and new_instance are required")
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
		return err
	}
	s.pushState()

	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_CLONE_INSTANCE, fleetName, destName, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunClone(fleetName, srcName, destName, false)
		return loadInstanceSnapshot(fleetName, destName), nil, err
	})
	return relay(j, stream)
}

func (s *service) StartInstance(req *fleetgrpc.StartInstanceRequest, stream fleetgrpc.FleetService_StartInstanceServer) error {
	if req.GetFleet() == "" || req.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "fleet and instance are required")
	}
	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_START_INSTANCE, req.GetFleet(), req.GetInstance(), time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunStart(req.GetFleet(), req.GetInstance())
		return loadInstanceSnapshot(req.GetFleet(), req.GetInstance()), nil, err
	})
	return relay(j, stream)
}

func (s *service) StopInstance(req *fleetgrpc.StopInstanceRequest, stream fleetgrpc.FleetService_StopInstanceServer) error {
	if req.GetFleet() == "" || req.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "fleet and instance are required")
	}
	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_STOP_INSTANCE, req.GetFleet(), req.GetInstance(), time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		err := jobRunStop(req.GetFleet(), req.GetInstance())
		return loadInstanceSnapshot(req.GetFleet(), req.GetInstance()), nil, err
	})
	return relay(j, stream)
}

// DestroyInstance tears down one instance, or (destroy_fleet) every instance in
// the fleet plus the fleet record. Best-effort: container/workspace failures
// become warnings, and the record is removed regardless.
func (s *service) DestroyInstance(req *fleetgrpc.DestroyInstanceRequest, stream fleetgrpc.FleetService_DestroyInstanceServer) error {
	fleetName := req.GetFleet()
	if fleetName == "" {
		return status.Error(codes.InvalidArgument, "fleet is required")
	}
	if !req.GetDestroyFleet() && req.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "instance is required unless destroy_fleet is set")
	}

	// Validate the target exists so a typo'd / stale name fails fast (the CLI
	// surfaces this as a non-zero exit) rather than a silent best-effort no-op.
	if st, err := state.Load(); err == nil {
		f, ok := st.Fleets[fleetName]
		if !ok {
			return status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
		}
		if !req.GetDestroyFleet() {
			if _, err := f.GetInstance(req.GetInstance()); err != nil {
				return status.Errorf(codes.NotFound, "instance %q not found in fleet %q", req.GetInstance(), fleetName)
			}
		}
	}

	target := req.GetInstance()
	j := s.jobs.start(fleetgrpc.JobKind_JOB_KIND_DESTROY_INSTANCE, fleetName, target, time.Now())
	go s.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		return nil, s.destroy(fleetName, target, req.GetDestroyFleet()), nil
	})
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
	// instances are down. Only meaningful when destroyFleet is set.
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
	// shared buildkit container and its cache directory. Single-instance
	// destroys leave the server up — its other instances may still use it.
	// Best-effort, so failures surface as warnings rather than aborting.
	if destroyFleet && buildkitEnabled {
		if err := buildkit.StopSharedServer(fleetName); err != nil {
			warnings = append(warnings, fmt.Sprintf("stop buildkit server: %v", err))
		}
		if err := os.RemoveAll(state.BuildkitDir(fleetName)); err != nil {
			warnings = append(warnings, fmt.Sprintf("remove buildkit dir %s: %v", state.BuildkitDir(fleetName), err))
		}
	}

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
