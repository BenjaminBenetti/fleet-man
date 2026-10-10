package server

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func requireGitForBranches(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runtimeByKey(rts []*fleetgrpc.InstanceRuntime) map[string]*fleetgrpc.InstanceRuntime {
	out := make(map[string]*fleetgrpc.InstanceRuntime, len(rts))
	for _, rt := range rts {
		out[runtimeKey(rt.GetFleet(), rt.GetInstance())] = rt
	}
	return out
}

// TestGetStateFreshBranchesReadsHostCheckouts drives GetState(fresh_branches)
// against real host checkouts, the devcontainer path: running AND stopped
// instances report the branch their checkout is on (the host read answers
// whether or not the instance runs), a detached HEAD reads as "" (present), a
// missing checkout gets no live read, and each newly seen named branch is
// persisted as LastBranch. Without fresh_branches the reply has no runtime.
func TestGetStateFreshBranchesReadsHostCheckouts(t *testing.T) {
	requireGitForBranches(t)
	isolateFleetDir(t)
	root := t.TempDir()
	running, stopped, detached := filepath.Join(root, "running"), filepath.Join(root, "stopped"), filepath.Join(root, "detached")
	gitInitOnBranch(t, running, "feature/running")
	gitInitOnBranch(t, stopped, "feature/stopped")
	gitInitOnBranch(t, detached, "main")
	gitIn(t, detached, "checkout", "-q", "--detach")
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "run", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "c1", WorkspaceDir: running},
			{Name: "stop", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer, ContainerID: "c2", WorkspaceDir: stopped, LastBranch: "old"},
			{Name: "det", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "c3", WorkspaceDir: detached, LastBranch: "main"},
			{Name: "gone", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer, ContainerID: "c4", WorkspaceDir: filepath.Join(root, "gone")},
			{Name: "creating", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := newService()
	ctx := context.Background()

	plain, err := svc.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(plain.GetRuntime()) != 0 {
		t.Fatalf("GetState without fresh_branches must not read branches, got %v", plain.GetRuntime())
	}

	reply, err := svc.GetState(ctx, &fleetgrpc.GetStateRequest{FreshBranches: true})
	if err != nil {
		t.Fatalf("GetState(fresh_branches): %v", err)
	}
	rts := runtimeByKey(reply.GetRuntime())
	for key, want := range map[string]string{
		"alpha/run":  "feature/running",
		"alpha/stop": "feature/stopped",
		"alpha/det":  "",
	} {
		rt := rts[key]
		if rt == nil || rt.CurrentBranch == nil || *rt.CurrentBranch != want {
			t.Errorf("%s: current_branch = %v, want %q present", key, rt.GetCurrentBranch(), want)
		}
	}
	if rt := rts["alpha/gone"]; rt == nil || rt.CurrentBranch != nil {
		t.Errorf("a missing checkout must give an entry with no live read, got %v", rt)
	}
	if _, ok := rts["alpha/creating"]; ok {
		t.Error("an instance with no workspace dir has nothing to read and gets no entry")
	}

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, inst := range st.Fleets["alpha"].Instances {
		got[inst.Name] = inst.LastBranch
	}
	want := map[string]string{"run": "feature/running", "stop": "feature/stopped", "det": "main", "gone": "", "creating": ""}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s LastBranch = %q, want %q (a detached HEAD or failed read keeps the last named branch)", name, got[name], w)
		}
	}
}

// TestFreshBranchesReuseRecentReads locks the GetState cache: a burst of calls
// reads each in-instance workspace once per branchCacheTTL, a branch switched
// in that window shows after the TTL, a rebuilt instance (new container ID) is
// read again at once, and a host checkout read is never reused.
func TestFreshBranchesReuseRecentReads(t *testing.T) {
	isolateFleetDir(t)
	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, ContainerID: "c1", WorkspaceDir: "/ws/i1"}
	st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}

	h := newHub()
	var reads atomic.Int32
	branch := "one"
	cacheable := true
	h.branches.read = func(context.Context, *fleet.Instance) branchRead {
		reads.Add(1)
		return branchRead{branch: branch, ok: true, cacheable: cacheable}
	}
	now := time.Unix(1000, 0)
	h.branches.now = func() time.Time { return now }
	ctx := context.Background()
	current := func() string { return h.freshBranches(ctx, st)[0].GetCurrentBranch() }

	if got := current(); got != "one" || reads.Load() != 1 {
		t.Fatalf("first call: %q after %d reads; want one after 1", got, reads.Load())
	}
	branch = "two"
	now = now.Add(branchCacheTTL - time.Second)
	if got := current(); got != "one" || reads.Load() != 1 {
		t.Fatalf("within the TTL: %q after %d reads; want the cached one after 1", got, reads.Load())
	}
	now = now.Add(2 * time.Second)
	if got := current(); got != "two" || reads.Load() != 2 {
		t.Fatalf("after the TTL: %q after %d reads; want two after 2", got, reads.Load())
	}
	branch = "three"
	inst.ContainerID = "c2"
	if got := current(); got != "three" || reads.Load() != 3 {
		t.Fatalf("after a rebuild: %q after %d reads; want three after 3", got, reads.Load())
	}

	cacheable = false
	now = now.Add(branchCacheTTL)
	current()
	branch = "four"
	if got := current(); got != "four" || reads.Load() != 5 {
		t.Fatalf("host reads: %q after %d reads; want four after 5 (never reused)", got, reads.Load())
	}
}

// TestReadBranchCachesOnlyInInstanceReads checks which reads the hub marks
// cacheable: a backend's own WorkspaceReader (a trip into the instance) is, the
// host checkout fallback every current backend uses is not.
func TestReadBranchCachesOnlyInInstanceReads(t *testing.T) {
	h := newHub()
	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer, WorkspaceDir: t.TempDir()}
	if rd := h.readBranch(context.Background(), inst); !rd.ok || rd.cacheable {
		t.Fatalf("devcontainer host read = %+v; want ok and not cacheable", rd)
	}
}

// TestBranchResolverBoundsConcurrencyAndTime checks the reads run in parallel
// but never more than branchReadConcurrency at once, each under its own
// deadline, and that the cache only keeps the instances it was last asked for.
func TestBranchResolverBoundsConcurrencyAndTime(t *testing.T) {
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	deadlines := 0
	r := newBranchResolver(func(ctx context.Context, inst *fleet.Instance) branchRead {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if _, ok := ctx.Deadline(); ok {
			mu.Lock()
			deadlines++
			mu.Unlock()
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return branchRead{branch: inst.Name, ok: true}
	})

	var targets []branchTarget
	for i := 0; i < 3*branchReadConcurrency; i++ {
		targets = append(targets, branchTarget{"alpha", &fleet.Instance{Name: string(rune('a' + i)), WorkspaceDir: "/ws"}})
	}
	reads := r.resolve(context.Background(), targets, 0)
	if len(reads) != len(targets) {
		t.Fatalf("got %d reads, want %d", len(reads), len(targets))
	}
	for _, tg := range targets {
		if rd := reads[tg.key()]; !rd.ok || rd.branch != tg.inst.Name {
			t.Fatalf("%s: read %+v", tg.key(), rd)
		}
	}
	if p := peak.Load(); p > branchReadConcurrency || p < 2 {
		t.Fatalf("peak concurrency %d, want 2..%d", p, branchReadConcurrency)
	}
	if deadlines != len(targets) {
		t.Fatalf("%d of %d reads had a deadline", deadlines, len(targets))
	}

	r.resolve(context.Background(), targets[:1], 0)
	if len(r.entries) != 1 {
		t.Fatalf("cache kept %d entries after asking for 1 instance", len(r.entries))
	}

	// A read cut short by its caller is not an answer to reuse.
	cut := newBranchResolver(func(ctx context.Context, inst *fleet.Instance) branchRead {
		return branchRead{ok: ctx.Err() == nil, cacheable: true}
	})
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cut.resolve(ended, targets[:1], 0)
	if e := cut.entries[targets[0].key()]; e.read.cacheable {
		t.Fatal("a read whose ctx ended must not be cached for reuse")
	}
}

// TestBranchPassPublishesRuntimeBranches checks the TUI path: the poller pass
// sets current_branch for every instance with a workspace (running or not),
// clears it when a later read can't reach the workspace, and records LastBranch.
func TestBranchPassPublishesRuntimeBranches(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "run", Status: fleet.StatusRunning, ContainerID: "c1", WorkspaceDir: "/ws/run"},
			{Name: "stop", Status: fleet.StatusStopped, ContainerID: "c2", WorkspaceDir: "/ws/stop"},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	readable := true
	h.branches.read = func(_ context.Context, inst *fleet.Instance) branchRead {
		if !readable && inst.Status == fleet.StatusStopped {
			return branchRead{}
		}
		return branchRead{branch: "feature/" + inst.Name, ok: true}
	}
	snapshot := func() map[string]*fleetgrpc.InstanceRuntime {
		out := make(chan map[string]*fleetgrpc.InstanceRuntime, 1)
		h.post(func(h *hub) {
			m := make(map[string]*fleetgrpc.InstanceRuntime)
			for k, r := range h.runtime {
				m[k] = cloneRuntime(r)
			}
			out <- m
		})
		return <-out
	}

	branchPass(ctx, h)
	rt := snapshot()
	if got := rt["alpha/run"].GetCurrentBranch(); got != "feature/run" {
		t.Fatalf("running current_branch = %q", got)
	}
	if got := rt["alpha/stop"].GetCurrentBranch(); got != "feature/stop" {
		t.Fatalf("stopped current_branch = %q (the host checkout is readable while stopped)", got)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Fleets["alpha"].Instances[1].LastBranch; got != "feature/stop" {
		t.Fatalf("LastBranch = %q, want feature/stop", got)
	}

	readable = false
	branchPass(ctx, h)
	if r := snapshot()["alpha/stop"]; r.CurrentBranch != nil {
		t.Fatalf("an unreadable workspace must clear current_branch so clients fall back, got %q", r.GetCurrentBranch())
	}
}

// TestMCPListReportsCurrentBranch checks fleet_list asks for fresh branches:
// current_branch is the workspace's checked-out branch (read on the daemon),
// falling back to the last branch seen when the workspace can't be read, while
// branch stays the requested one.
func TestMCPListReportsCurrentBranch(t *testing.T) {
	requireGitForBranches(t)
	isolateFleetDir(t)
	ws := filepath.Join(t.TempDir(), "ws")
	gitInitOnBranch(t, ws, "feature/switched")
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "cid1", WorkspaceDir: ws, Branch: "main"},
			{Name: "i2", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer, ContainerID: "cid2",
				WorkspaceDir: filepath.Join(ws, "missing"), Branch: "main", LastBranch: "feature/last"},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cs := mcpConnect(t, newService())

	var list FleetListOutput
	callJSON(t, cs, "fleet_list", map[string]any{"fleet": "alpha"}, &list)
	got := map[string]mcpInstance{}
	for _, inst := range list.Instances {
		got[inst.Instance] = inst
	}
	if i1 := got["i1"]; i1.CurrentBranch != "feature/switched" || i1.Branch != "main" {
		t.Fatalf("i1: current_branch=%q branch=%q; want feature/switched, main", i1.CurrentBranch, i1.Branch)
	}
	if i2 := got["i2"]; i2.CurrentBranch != "feature/last" || i2.Branch != "main" {
		t.Fatalf("i2: current_branch=%q branch=%q; want feature/last, main", i2.CurrentBranch, i2.Branch)
	}
}

// workspaceReaderBackend is a backend whose workspace lives in the instance:
// reads go through its WorkspaceReader, never the host directory.
type workspaceReaderBackend struct {
	backend.Backend
	files map[string]string
}

func (workspaceReaderBackend) Branch(context.Context, string) (string, bool) { return "", false }

func (b workspaceReaderBackend) ReadWorkspaceFile(_ context.Context, _, relPath string) ([]byte, error) {
	if body, ok := b.files[relPath]; ok {
		return []byte(body), nil
	}
	return nil, fs.ErrNotExist
}

// TestBrowserCustomizationsReadThroughTheWorkspaceReader checks the browser
// RPCs' devcontainer.json read reaches the workspace through the backend's
// WorkspaceReader: a backend that implements one is read there (the host dir
// is not consulted), every other backend reads the host checkout as before.
func TestBrowserCustomizationsReadThroughTheWorkspaceReader(t *testing.T) {
	ctx := context.Background()
	inInstance := workspaceReaderBackend{files: map[string]string{
		".devcontainer/devcontainer.json": `{"customizations":{"fleet":{"browser":{"initialUrl":"http://in-instance"}}}}`,
	}}
	fc, err := loadWorkspaceCustomizations(ctx, inInstance, filepath.Join(t.TempDir(), "no-host-checkout"))
	if err != nil || fc.Browser.InitialURL != "http://in-instance" {
		t.Fatalf("reader backend: got %q, %v; want the in-instance file", fc.Browser.InitialURL, err)
	}

	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, ".devcontainer.json"), []byte(`{"customizations":{"fleet":{"browser":{"initialUrl":"http://host"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err = loadWorkspaceCustomizations(ctx, backendutil.New(fleet.BackendDevcontainer, false), host)
	if err != nil || fc.Browser.InitialURL != "http://host" {
		t.Fatalf("devcontainer: got %q, %v; want the host checkout's file", fc.Browser.InitialURL, err)
	}
}
