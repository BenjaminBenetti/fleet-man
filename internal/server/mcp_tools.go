package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/status"
)

// mcp_tools.go defines the fleet MCP tools: their typed inputs/outputs (the
// jsonschema is inferred from the struct fields) and handlers. Each handler
// reuses the in-process *service methods so MCP behaves identically to the gRPC
// CLI path. A returned Go error becomes an MCP TOOL error (visible to the
// model), not a transport error; a non-zero command exit is reported as data,
// not an error.
//
// Names are flat and prefixed `fleet_` (the MCP spec restricts tool names to
// [A-Za-z0-9_.-]). cwd-based fleet/instance inference — a host/client concept —
// is dropped: tools take explicit fleet and instance arguments.
//
// The slow, job-shaped lifecycle tools (fleet_up, fleet_clone, fleet_down,
// fleet_destroy_fleet) are ASYNC-FIRST (issue #134): they start the server-
// owned job and return a {job_id, done:false} handle within seconds, instead of
// blocking past the MCP client's tool-call timeout (provisioning takes
// minutes). Completion, failure, and warnings are polled via fleet_job_status
// (or fleet_list's status/error). wait:true opts back into blocking. fleet_start
// and fleet_stop stay blocking — they finish in seconds.

// registerMCPTools wires every fleet tool onto srv.
func registerMCPTools(srv *mcp.Server, s *service) {
	// --- read ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_list",
		Description: "List devcontainer instances across fleets. Optionally filter to one fleet.",
	}, s.mcpList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_status",
		Description: "Summarize fleets and instance counts (running/stopped/other).",
	}, s.mcpStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_version",
		Description: "Report the running fleet daemon version and liveness (pid, start time).",
	}, s.mcpVersion)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_logs",
		Description: "Return an instance's current container logs (non-following). Use tail to cap to the last N lines.",
	}, s.mcpLogs)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_restore_backup",
		Description: "Documentation only (restores nothing): explain where fleetd's hourly state backups live, list the archives that exist, describe what each contains, and give the manual restore procedure (stop fleetd, then unpack the chosen archive over ~/.fleet). Use this when asked to restore fleet state from a backup.",
	}, s.mcpRestoreBackup)

	// --- lifecycle ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_up",
		Description: "Create (provision) a new instance in a fleet. Returns immediately with a job_id to poll via fleet_job_status (provisioning takes minutes); pass wait:true to block until done instead. Provide remote (git URL) when creating the first instance of a new fleet.",
	}, s.mcpUp)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_start",
		Description: "Start a previously stopped instance's container. Blocks until started.",
	}, s.mcpStart)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_stop",
		Description: "Stop an instance's container, keeping the instance record. Blocks until stopped.",
	}, s.mcpStop)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_down",
		Description: "Stop and remove a single instance: tears down its container, removes its workspace, and deletes the record. Returns immediately with a job_id to poll via fleet_job_status; pass wait:true to block until done.",
	}, s.mcpDown)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_destroy_fleet",
		Description: "Destroy an entire fleet: tears down every instance and removes the fleet record. Returns immediately with a job_id to poll via fleet_job_status; pass wait:true to block until done.",
	}, s.mcpDestroyFleet)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_clone",
		Description: "Clone an existing instance into a new one within the same fleet, preserving its container state. Returns immediately with a job_id to poll via fleet_job_status; pass wait:true to block until done.",
	}, s.mcpClone)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_rebuild",
		Description: "Rebuild an instance's container in place (e.g. after editing its devcontainer config), preserving the workspace — the git checkout and uncommitted edits survive. Only devcontainer and codespaces backends support rebuild; coder does not. Returns immediately with a job_id to poll via fleet_job_status; pass wait:true to block until done.",
	}, s.mcpRebuild)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_job_status",
		Description: "Report the state of a lifecycle job (running, succeeded, or failed) with its error and warnings. Poll this to await a job started by fleet_up, fleet_clone, fleet_down, or fleet_destroy_fleet.",
	}, s.mcpJobStatus)

	// --- exec & sessions ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_exec",
		Description: "Run a one-shot command inside a running instance and capture stdout, stderr, and exit code. Not for long-running or interactive commands.",
	}, s.mcpExec)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_session_spawn",
		Description: "Create a new detached tmux session inside a running instance.",
	}, s.mcpSessionSpawn)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_session_exec",
		Description: "Type a command into an existing tmux session (send-keys + Enter). Fire-and-forget; read the session afterwards to see output.",
	}, s.mcpSessionExec)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_session_read",
		Description: "Capture the current screen contents of a tmux session inside an instance.",
	}, s.mcpSessionRead)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_session_list",
		Description: "List the tmux sessions inside a running instance (name, window count, creation time, attached). Returns an empty list when the instance has no sessions.",
	}, s.mcpSessionList)

	// --- automation (agents & triggers) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_automation_list",
		Description: "Read a fleet's automation config: its agents (automation workers) and triggers (what fires them). Returns {agents, triggers}.",
	}, s.mcpAutomationList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_agent_create",
		Description: "Create an automation agent in a fleet: a worker definition (launch command, system prompt, env backend) that triggers activate. Returns the fleet's resulting automation config.",
	}, s.mcpAgentCreate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_agent_update",
		Description: "Update an existing automation agent. Only the fields you pass change (omit one to keep its current value); use new_name to rename (trigger references follow). Returns the resulting automation config.",
	}, s.mcpAgentUpdate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_agent_delete",
		Description: "Delete an automation agent. Fails if a trigger still references it — detach it from that trigger first. Returns the resulting automation config.",
	}, s.mcpAgentDelete)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_trigger_create",
		Description: "Create an automation trigger in a fleet: it activates one or more agents (with a prompt) on a cron schedule or a gateway webhook event. Returns the fleet's resulting automation config.",
	}, s.mcpTriggerCreate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_trigger_update",
		Description: "Update an existing automation trigger. Only the fields you pass change (omit one to keep its current value); use new_name to rename. Returns the resulting automation config.",
	}, s.mcpTriggerUpdate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_trigger_delete",
		Description: "Delete an automation trigger. Returns the fleet's resulting automation config.",
	}, s.mcpTriggerDelete)
}

// --- shared shapes ---

// mcpInstance is the JSON-friendly view of an instance returned by the read and
// lifecycle tools. It surfaces the persisted record fields (no live git HEAD
// lookup, which would shell out per instance); branch is the REQUESTED branch.
type mcpInstance struct {
	Fleet        string `json:"fleet"`
	Instance     string `json:"instance"`
	Status       string `json:"status"`
	ContainerID  string `json:"container_id,omitempty"`
	Backend      string `json:"backend,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Tag          string `json:"tag,omitempty"`
	WorkspaceDir string `json:"workspace_dir,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	Error        string `json:"error,omitempty"`
}

func toMCPInstance(fleetName string, inst *fleetgrpc.Instance) *mcpInstance {
	if inst == nil {
		return nil
	}
	m := &mcpInstance{
		Fleet:        fleetName,
		Instance:     inst.GetName(),
		Status:       inst.GetStatus().Display(),
		ContainerID:  inst.GetContainerId(),
		Backend:      inst.GetBackend().Display(),
		Branch:       inst.GetBranch(),
		Tag:          inst.GetTag(),
		WorkspaceDir: inst.GetWorkspaceDir(),
		Error:        inst.GetError(),
	}
	if ts := inst.GetCreatedAt(); ts != nil {
		m.CreatedAt = ts.AsTime().UTC().Format(time.RFC3339)
	}
	return m
}

// FleetJobOutput is the result of a lifecycle tool. An async kickoff (the
// default for fleet_up/fleet_clone/fleet_down/fleet_destroy_fleet) returns
// done=false the moment the job starts: job_id is the handle to poll via
// fleet_job_status, and instance is the transitional record (creating/cloning/
// deleting; absent for fleet-wide jobs). A blocking call (wait:true, and always
// fleet_start/fleet_stop) returns done=true with the final record. Warnings are
// non-fatal issues from an otherwise-successful job (e.g. a best-effort
// teardown step).
type FleetJobOutput struct {
	JobID    string       `json:"job_id,omitempty"`
	Done     bool         `json:"done"`
	Success  bool         `json:"success,omitempty"`
	Instance *mcpInstance `json:"instance,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
}

// jobResult turns a completed lifecycle outcome into a tool result: a failed
// job becomes a tool error (any warnings folded into the message); a successful
// one returns the final instance + warnings.
func jobResult(fleetName, jobID string, final *fleetgrpc.Instance, warnings []string, err error) (*mcp.CallToolResult, FleetJobOutput, error) {
	if err != nil {
		if len(warnings) > 0 {
			return nil, FleetJobOutput{}, fmt.Errorf("%w (warnings: %s)", mcpErr(err), strings.Join(warnings, "; "))
		}
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	return nil, FleetJobOutput{JobID: jobID, Done: true, Success: true, Instance: toMCPInstance(fleetName, final), Warnings: warnings}, nil
}

// asyncJobResult is the immediate return of an async lifecycle kickoff: the job
// handle plus the instance's current transitional record (nil for fleet-wide
// jobs, or if the job already removed the record). The caller discovers
// completion by polling fleet_job_status {job_id} or fleet_list.
func asyncJobResult(fleetName string, j *job) (*mcp.CallToolResult, FleetJobOutput, error) {
	return nil, FleetJobOutput{
		JobID:    j.summary.GetJobId(),
		Instance: toMCPInstance(fleetName, loadInstanceSnapshot(fleetName, j.summary.GetInstance())),
	}, nil
}

// FleetInstanceInput identifies one instance.
type FleetInstanceInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
}

// --- read tools ---

type FleetListInput struct {
	Fleet string `json:"fleet,omitempty" jsonschema:"optional fleet name to filter by; empty lists all fleets"`
}

type FleetListOutput struct {
	Instances []mcpInstance `json:"instances"`
}

func (s *service) mcpList(ctx context.Context, _ *mcp.CallToolRequest, in FleetListInput) (*mcp.CallToolResult, FleetListOutput, error) {
	reply, err := s.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return nil, FleetListOutput{}, mcpErr(err)
	}
	out := FleetListOutput{Instances: []mcpInstance{}}
	for _, name := range sortedFleetNames(reply.GetState()) {
		if in.Fleet != "" && name != in.Fleet {
			continue
		}
		f := reply.GetState().GetFleets()[name]
		for _, inst := range f.GetInstances() {
			out.Instances = append(out.Instances, *toMCPInstance(name, inst))
		}
	}
	return nil, out, nil
}

type FleetStatusEntry struct {
	Fleet   string `json:"fleet"`
	Remote  string `json:"remote,omitempty"`
	Total   int    `json:"total"`
	Running int    `json:"running"`
	Stopped int    `json:"stopped"`
	Other   int    `json:"other"`
}

type FleetStatusOutput struct {
	Fleets         []FleetStatusEntry `json:"fleets"`
	TotalFleets    int                `json:"total_fleets"`
	TotalInstances int                `json:"total_instances"`
	Running        int                `json:"running"`
	Stopped        int                `json:"stopped"`
	Other          int                `json:"other"`
}

func (s *service) mcpStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, FleetStatusOutput, error) {
	reply, err := s.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return nil, FleetStatusOutput{}, mcpErr(err)
	}
	out := FleetStatusOutput{Fleets: []FleetStatusEntry{}}
	for _, name := range sortedFleetNames(reply.GetState()) {
		f := reply.GetState().GetFleets()[name]
		entry := FleetStatusEntry{Fleet: name, Remote: f.GetRemote()}
		for _, inst := range f.GetInstances() {
			entry.Total++
			switch inst.GetStatus() {
			case fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING:
				entry.Running++
			case fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPED:
				entry.Stopped++
			default:
				entry.Other++
			}
		}
		out.Fleets = append(out.Fleets, entry)
		out.TotalFleets++
		out.TotalInstances += entry.Total
		out.Running += entry.Running
		out.Stopped += entry.Stopped
		out.Other += entry.Other
	}
	return nil, out, nil
}

type FleetVersionOutput struct {
	Version   string `json:"version"`
	Pid       int64  `json:"pid"`
	StartedAt string `json:"started_at,omitempty"`
}

func (s *service) mcpVersion(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, FleetVersionOutput, error) {
	reply, err := s.Hello(ctx, &fleetgrpc.HelloRequest{})
	if err != nil {
		return nil, FleetVersionOutput{}, mcpErr(err)
	}
	out := FleetVersionOutput{Version: reply.GetServerVersion(), Pid: reply.GetPid()}
	if ts := reply.GetStartedAt(); ts != nil {
		out.StartedAt = ts.AsTime().UTC().Format(time.RFC3339)
	}
	return nil, out, nil
}

type FleetLogsInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Tail     int    `json:"tail,omitempty" jsonschema:"if > 0, return only the last N lines"`
}

type FleetLogsOutput struct {
	Logs  string `json:"logs"`
	Lines int    `json:"lines"`
}

func (s *service) mcpLogs(ctx context.Context, _ *mcp.CallToolRequest, in FleetLogsInput) (*mcp.CallToolResult, FleetLogsOutput, error) {
	// follow=false: the backend logs command dumps existing logs then exits, so
	// the collector sees EOF and Logs returns — a bounded, one-shot read.
	c := &streamCollector[fleetgrpc.LogLine]{ctx: ctx}
	if err := s.Logs(&fleetgrpc.LogsRequest{Fleet: in.Fleet, Instance: in.Instance, Follow: false}, c); err != nil {
		return nil, FleetLogsOutput{}, mcpErr(err)
	}
	lines := make([]string, len(c.events))
	for i, ll := range c.events {
		lines[i] = ll.GetLine()
	}
	if in.Tail > 0 && len(lines) > in.Tail {
		lines = lines[len(lines)-in.Tail:]
	}
	return nil, FleetLogsOutput{Logs: strings.Join(lines, "\n"), Lines: len(lines)}, nil
}

// --- lifecycle tools ---

type FleetUpInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Remote   string `json:"remote,omitempty" jsonschema:"git remote URL; required only when creating the first instance of a new fleet"`
	Branch   string `json:"branch,omitempty" jsonschema:"git branch to check out; defaults to the repository's default branch"`
	Backend  string `json:"backend,omitempty" jsonschema:"backend type: devcontainer (default), coder, or codespaces"`
	Wait     bool   `json:"wait,omitempty" jsonschema:"block until provisioning completes instead of returning a job handle immediately; provisioning takes minutes, so only set this with a generous tool-call timeout"`
}

func (s *service) mcpUp(ctx context.Context, _ *mcp.CallToolRequest, in FleetUpInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Instance == "" {
		return nil, FleetJobOutput{}, errors.New("fleet and instance are required")
	}
	backend := fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED
	if in.Backend != "" {
		bt, err := fleet.ParseBackendType(in.Backend)
		if err != nil {
			return nil, FleetJobOutput{}, err
		}
		backend = protoBackend(bt)
	}
	// Verbose is left false (unlike `fleet up`): MCP returns only the final
	// JobDone result, never a live provisioning stream, so verbose output would
	// just be discarded.
	req := &fleetgrpc.CreateInstanceRequest{Fleet: in.Fleet, Instance: in.Instance, Backend: backend}
	if in.Remote != "" {
		req.Remote = &in.Remote
	}
	if in.Branch != "" {
		req.Branch = &in.Branch
	}
	j, err := s.startCreateInstanceJob(req, false)
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	if !in.Wait {
		return asyncJobResult(in.Fleet, j)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

func (s *service) mcpStart(ctx context.Context, _ *mcp.CallToolRequest, in FleetInstanceInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Instance == "" {
		return nil, FleetJobOutput{}, errors.New("fleet and instance are required")
	}
	j, err := s.startStartInstanceJob(&fleetgrpc.StartInstanceRequest{Fleet: in.Fleet, Instance: in.Instance})
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

func (s *service) mcpStop(ctx context.Context, _ *mcp.CallToolRequest, in FleetInstanceInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Instance == "" {
		return nil, FleetJobOutput{}, errors.New("fleet and instance are required")
	}
	j, err := s.startStopInstanceJob(&fleetgrpc.StopInstanceRequest{Fleet: in.Fleet, Instance: in.Instance})
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

type FleetDownInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Wait     bool   `json:"wait,omitempty" jsonschema:"block until the teardown completes instead of returning a job handle immediately"`
}

func (s *service) mcpDown(ctx context.Context, _ *mcp.CallToolRequest, in FleetDownInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Instance == "" {
		return nil, FleetJobOutput{}, errors.New("fleet and instance are required")
	}
	instance := in.Instance
	j, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: in.Fleet, Instance: &instance})
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	if !in.Wait {
		return asyncJobResult(in.Fleet, j)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

type FleetDestroyFleetInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
	Wait  bool   `json:"wait,omitempty" jsonschema:"block until the teardown completes instead of returning a job handle immediately"`
}

func (s *service) mcpDestroyFleet(ctx context.Context, _ *mcp.CallToolRequest, in FleetDestroyFleetInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" {
		return nil, FleetJobOutput{}, errors.New("fleet is required")
	}
	j, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: in.Fleet, DestroyFleet: true})
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	if !in.Wait {
		return asyncJobResult(in.Fleet, j)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

type FleetCloneInput struct {
	Fleet       string `json:"fleet" jsonschema:"fleet name"`
	Source      string `json:"source" jsonschema:"name of the existing instance to clone"`
	Destination string `json:"destination" jsonschema:"name for the new cloned instance"`
	DisplayName string `json:"display_name,omitempty" jsonschema:"optional display name for the clone"`
	Tag         string `json:"tag,omitempty" jsonschema:"optional tag override for the clone"`
	Color       string `json:"color,omitempty" jsonschema:"optional color override for the clone"`
	Branch      string `json:"branch,omitempty" jsonschema:"optional branch override for the clone"`
	Wait        bool   `json:"wait,omitempty" jsonschema:"block until the clone completes instead of returning a job handle immediately; cloning can take minutes"`
}

func (s *service) mcpClone(ctx context.Context, _ *mcp.CallToolRequest, in FleetCloneInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Source == "" || in.Destination == "" {
		return nil, FleetJobOutput{}, errors.New("fleet, source and destination are required")
	}
	req := &fleetgrpc.CloneInstanceRequest{Fleet: in.Fleet, SourceInstance: in.Source, NewInstance: in.Destination}
	if in.DisplayName != "" {
		req.NewDisplayName = &in.DisplayName
	}
	if in.Tag != "" {
		req.TagOverride = &in.Tag
	}
	if in.Color != "" {
		req.ColorOverride = &in.Color
	}
	if in.Branch != "" {
		req.BranchOverride = &in.Branch
	}
	j, err := s.startCloneInstanceJob(req)
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	if !in.Wait {
		return asyncJobResult(in.Fleet, j)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

type FleetRebuildInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Wait     bool   `json:"wait,omitempty" jsonschema:"block until the rebuild completes instead of returning a job handle immediately; rebuilding can take minutes"`
}

func (s *service) mcpRebuild(ctx context.Context, _ *mcp.CallToolRequest, in FleetRebuildInput) (*mcp.CallToolResult, FleetJobOutput, error) {
	if in.Fleet == "" || in.Instance == "" {
		return nil, FleetJobOutput{}, errors.New("fleet and instance are required")
	}
	j, err := s.startRebuildInstanceJob(&fleetgrpc.RebuildInstanceRequest{Fleet: in.Fleet, Instance: in.Instance})
	if err != nil {
		return nil, FleetJobOutput{}, mcpErr(err)
	}
	if !in.Wait {
		return asyncJobResult(in.Fleet, j)
	}
	jctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	final, warnings, err := awaitJob(jctx, j)
	return jobResult(in.Fleet, j.summary.GetJobId(), final, warnings, err)
}

type FleetJobStatusInput struct {
	JobID string `json:"job_id" jsonschema:"job id returned by a lifecycle tool"`
}

// FleetJobStatusOutput reports one lifecycle job. State is running until the
// job finishes, then succeeded or failed (with Error). Result is the final
// instance record from the job, when it produced one. A failed job is DATA
// here (state:"failed"), not a tool error — polling must be able to read the
// failure.
type FleetJobStatusOutput struct {
	JobID     string       `json:"job_id"`
	Kind      string       `json:"kind"`
	Fleet     string       `json:"fleet"`
	Instance  string       `json:"instance,omitempty"`
	State     string       `json:"state"`
	StartedAt string       `json:"started_at,omitempty"`
	Ms        int64        `json:"ms,omitempty"`
	Error     string       `json:"error,omitempty"`
	Warnings  []string     `json:"warnings,omitempty"`
	Result    *mcpInstance `json:"result,omitempty"`
}

func (s *service) mcpJobStatus(_ context.Context, _ *mcp.CallToolRequest, in FleetJobStatusInput) (*mcp.CallToolResult, FleetJobStatusOutput, error) {
	if in.JobID == "" {
		return nil, FleetJobStatusOutput{}, errors.New("job_id is required")
	}
	j := s.jobs.get(in.JobID)
	if j == nil {
		return nil, FleetJobStatusOutput{}, fmt.Errorf("job %q not found (it may pre-date a daemon restart, or its result expired); check the instance's status and error via fleet_list instead", in.JobID)
	}
	out := FleetJobStatusOutput{
		JobID:    j.summary.GetJobId(),
		Kind:     jobKindName(j.summary.GetKind()),
		Fleet:    j.summary.GetFleet(),
		Instance: j.summary.GetInstance(),
		State:    "running",
	}
	if ts := j.summary.GetStartedAt(); ts != nil {
		out.StartedAt = ts.AsTime().UTC().Format(time.RFC3339)
	}
	if d := j.outcome(); d != nil {
		out.State = "succeeded"
		if !d.GetSuccess() {
			out.State = "failed"
			out.Error = d.GetError()
		}
		out.Ms = d.GetMs()
		out.Warnings = d.GetWarnings()
		out.Result = toMCPInstance(j.summary.GetFleet(), d.GetInstance())
	}
	return nil, out, nil
}

// --- exec & session tools ---

type FleetExecInput struct {
	Fleet    string   `json:"fleet" jsonschema:"fleet name"`
	Instance string   `json:"instance" jsonschema:"instance name"`
	Command  []string `json:"command" jsonschema:"command argv to run (not a shell string); e.g. [\"ls\", \"-la\"]"`
}

type FleetExecOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (s *service) mcpExec(ctx context.Context, _ *mcp.CallToolRequest, in FleetExecInput) (*mcp.CallToolResult, FleetExecOutput, error) {
	if len(in.Command) == 0 {
		return nil, FleetExecOutput{}, errors.New("command is required")
	}
	inst, err := s.runningInstance(in.Fleet, in.Instance)
	if err != nil {
		return nil, FleetExecOutput{}, err
	}
	// Kill the command if the daemon shuts down or the MCP session closes, so a
	// hung command can't pin the request (and thus daemon teardown).
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	var outBuf, errBuf bytes.Buffer
	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir, in.Command).Cmd
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := runCmd(cctx, cmd)
	exitCode := 0
	if runErr != nil {
		// A non-zero exit is reported as data, not a tool error; only a genuine
		// failure to launch the command is an error.
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return nil, FleetExecOutput{}, fmt.Errorf("exec: %w", runErr)
		}
	}
	return nil, FleetExecOutput{Stdout: outBuf.String(), Stderr: errBuf.String(), ExitCode: exitCode}, nil
}

type FleetSessionSpawnInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Session  string `json:"session" jsonschema:"name for the new tmux session"`
}

type FleetSessionMessageOutput struct {
	Message string `json:"message"`
}

func (s *service) mcpSessionSpawn(ctx context.Context, _ *mcp.CallToolRequest, in FleetSessionSpawnInput) (*mcp.CallToolResult, FleetSessionMessageOutput, error) {
	if in.Session == "" {
		return nil, FleetSessionMessageOutput{}, errors.New("session is required")
	}
	inst, err := s.runningInstance(in.Fleet, in.Instance)
	if err != nil {
		return nil, FleetSessionMessageOutput{}, err
	}
	// Canonicalize to the TUI's group naming convention (<instance>~<name>),
	// exactly like the CLI session commands — a bare-named session would
	// surface as a pseudo-group in the TUI and splitting it would mint a
	// duplicate real group with the same name.
	sessionName := tui.ResolveSessionName(in.Instance, in.Session)
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	snippet := dotfiles.TmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s`, dotfiles.ShQuote(sessionName))
	if out, err := runContainerShell(cctx, inst, snippet); err != nil {
		return nil, FleetSessionMessageOutput{}, fmt.Errorf("spawn session %q: %w: %s", in.Session, err, strings.TrimSpace(out))
	}
	return nil, FleetSessionMessageOutput{Message: fmt.Sprintf("created tmux session %q in %s/%s", sessionName, in.Fleet, in.Instance)}, nil
}

type FleetSessionExecInput struct {
	Fleet    string `json:"fleet" jsonschema:"fleet name"`
	Instance string `json:"instance" jsonschema:"instance name"`
	Session  string `json:"session" jsonschema:"name of the existing tmux session"`
	Command  string `json:"command" jsonschema:"command to type into the session's shell"`
}

func (s *service) mcpSessionExec(ctx context.Context, _ *mcp.CallToolRequest, in FleetSessionExecInput) (*mcp.CallToolResult, FleetSessionMessageOutput, error) {
	if in.Session == "" || in.Command == "" {
		return nil, FleetSessionMessageOutput{}, errors.New("session and command are required")
	}
	inst, err := s.runningInstance(in.Fleet, in.Instance)
	if err != nil {
		return nil, FleetSessionMessageOutput{}, err
	}
	// Resolve the short name the agent uses to the same canonical session
	// fleet_session_spawn created (no-op for already-canonical names).
	sessionName := tui.ResolveSessionName(in.Instance, in.Session)
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	snippet := fmt.Sprintf(`tmux send-keys -t %s %s Enter`, dotfiles.ShQuote(sessionName), dotfiles.ShQuote(in.Command))
	if out, err := runContainerShell(cctx, inst, snippet); err != nil {
		return nil, FleetSessionMessageOutput{}, fmt.Errorf("exec in session %q: %w: %s", in.Session, err, strings.TrimSpace(out))
	}
	return nil, FleetSessionMessageOutput{Message: fmt.Sprintf("sent command to session %q; read the session to see output", sessionName)}, nil
}

type FleetSessionReadInput struct {
	Fleet      string `json:"fleet" jsonschema:"fleet name"`
	Instance   string `json:"instance" jsonschema:"instance name"`
	Session    string `json:"session" jsonschema:"name of the tmux session to read"`
	Scrollback int    `json:"scrollback,omitempty" jsonschema:"scrollback lines: 0 for the visible pane only, positive N for the last N history lines, negative for full history"`
}

type FleetSessionReadOutput struct {
	Content string `json:"content"`
}

func (s *service) mcpSessionRead(ctx context.Context, _ *mcp.CallToolRequest, in FleetSessionReadInput) (*mcp.CallToolResult, FleetSessionReadOutput, error) {
	if in.Session == "" {
		return nil, FleetSessionReadOutput{}, errors.New("session is required")
	}
	inst, err := s.runningInstance(in.Fleet, in.Instance)
	if err != nil {
		return nil, FleetSessionReadOutput{}, err
	}
	// Same short-name resolution as fleet_session_spawn/exec.
	sessionName := tui.ResolveSessionName(in.Instance, in.Session)
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	// Translate scrollback into tmux's -S start-line: negative => full history
	// (-S -), positive => last N lines (-S -N), zero => visible pane only.
	startFlag := ""
	switch {
	case in.Scrollback < 0:
		startFlag = "-S - "
	case in.Scrollback > 0:
		startFlag = fmt.Sprintf("-S -%d ", in.Scrollback)
	}
	snippet := fmt.Sprintf(`tmux capture-pane -p %s-t %s`, startFlag, dotfiles.ShQuote(sessionName))
	out, err := runContainerShell(cctx, inst, snippet)
	if err != nil {
		return nil, FleetSessionReadOutput{}, fmt.Errorf("read session %q: %w: %s", in.Session, err, strings.TrimSpace(out))
	}
	return nil, FleetSessionReadOutput{Content: out}, nil
}

// fleetSessionListFormat lists every tmux session, one per line, as
// `windows:attached:created:name`. session_name is placed LAST so a name
// containing the ':' delimiter (tmux allows it) can't shift the fixed leading
// fields — parseMCPSessions caps the split at 4 and keeps the remainder as the
// name. `2>/dev/null` drops tmux's "no server running" stderr; the caller appends
// `|| true` so that exit-1 (no sessions) collapses to empty output, not an error.
const fleetSessionListFormat = `tmux list-sessions -F "#{session_windows}:#{session_attached}:#{session_created}:#{session_name}" 2>/dev/null`

// FleetSession is the JSON-friendly view of one tmux session.
type FleetSession struct {
	Session   string `json:"session"`
	Windows   int    `json:"windows"`
	CreatedAt string `json:"created_at,omitempty"`
	Attached  bool   `json:"attached"`
}

type FleetSessionListOutput struct {
	Sessions []FleetSession `json:"sessions"`
}

// mcpSessionList enumerates the tmux sessions inside a running instance. A
// running instance with no sessions yields an empty list (not an error); a
// not-running instance is rejected up front by runningInstance.
func (s *service) mcpSessionList(ctx context.Context, _ *mcp.CallToolRequest, in FleetInstanceInput) (*mcp.CallToolResult, FleetSessionListOutput, error) {
	inst, err := s.runningInstance(in.Fleet, in.Instance)
	if err != nil {
		return nil, FleetSessionListOutput{}, err
	}
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	// `|| true` makes "no tmux server" (exit 1) a successful empty read; a genuine
	// exec failure (e.g. the container vanished) still surfaces as a tool error.
	out, err := runContainerShell(cctx, inst, fleetSessionListFormat+" || true")
	if err != nil {
		return nil, FleetSessionListOutput{}, fmt.Errorf("list sessions in %s/%s: %w: %s", in.Fleet, in.Instance, err, strings.TrimSpace(out))
	}
	return nil, FleetSessionListOutput{Sessions: parseMCPSessions(out)}, nil
}

// parseMCPSessions parses fleetSessionListFormat output into the MCP session
// list. Blank/short lines are skipped; created (a Unix epoch) is rendered as
// RFC3339 UTC to match the other MCP timestamps, or omitted if unparseable.
func parseMCPSessions(output string) []FleetSession {
	sessions := []FleetSession{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 || parts[3] == "" {
			continue
		}
		windows, _ := strconv.Atoi(parts[0])
		// session_attached is the CLIENT COUNT, not a 0/1 flag: any positive count
		// means a client is attached; blank/unparseable counts as detached.
		attached, _ := strconv.Atoi(parts[1])
		sess := FleetSession{Session: parts[3], Windows: windows, Attached: attached > 0}
		if created, err := strconv.ParseInt(parts[2], 10, 64); err == nil && created > 0 {
			sess.CreatedAt = time.Unix(created, 0).UTC().Format(time.RFC3339)
		}
		sessions = append(sessions, sess)
	}
	return sessions
}

// --- helpers ---

// runningInstance resolves an instance and requires it to be running, returning
// a clear tool error otherwise (exec and session ops need a live container).
func (s *service) runningInstance(fleetName, instanceName string) (*fleet.Instance, error) {
	inst, err := resolveServerInstance(fleetName, instanceName)
	if err != nil {
		return nil, mcpErr(err)
	}
	if inst.Status != fleet.StatusRunning {
		return nil, fmt.Errorf("instance %s/%s is not running (status: %s)", fleetName, instanceName, inst.Status)
	}
	return inst, nil
}

// runContainerShell runs `sh -c <snippet>` inside the instance's container and
// returns the combined output. Session callers shell-quote untrusted names via
// dotfiles.ShQuote before building the snippet. The command is killed if ctx is
// cancelled (daemon shutdown / session close).
func runContainerShell(ctx context.Context, inst *fleet.Instance, snippet string) (string, error) {
	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir, []string{"sh", "-c", snippet}).Cmd
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := runCmd(ctx, cmd)
	return buf.String(), err
}

// runCmd runs cmd, killing its process if ctx is cancelled (daemon shutdown /
// session close). The kill watcher is started AFTER Start sets cmd.Process, and
// an already-cancelled ctx is rejected before Start, so a cancellation can never
// race a process into existence unkilled (the bounded-teardown contract). The
// caller sets cmd.Stdout/Stderr before calling.
func runCmd(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	return cmd.Wait()
}

// mcpErr strips the gRPC status wrapper ("rpc error: code = ... desc = ...")
// from an in-process service error so the message an MCP client (and the model
// reading it) sees is clean. Non-status errors pass through unchanged.
func mcpErr(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return errors.New(st.Message())
	}
	return err
}

// sortedFleetNames returns the fleet names in deterministic order (map iteration
// is random; the CLI doesn't sort, but a programmatic API should be stable).
func sortedFleetNames(st *fleetgrpc.State) []string {
	names := make([]string, 0, len(st.GetFleets()))
	for name := range st.GetFleets() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
