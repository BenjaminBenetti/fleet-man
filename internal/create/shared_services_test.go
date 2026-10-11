package create

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
)

// mountBackend answers the mount question provisioning asks. Every other
// method panics on the nil embedded interface, so a step that reaches into the
// instance when it should not fails loudly.
type mountBackend struct {
	backend.Backend
	shared bool
}

func (b mountBackend) SupportsSharedMounts() bool { return b.shared }

// channelBackend is a mountBackend that carries the control channel itself.
type channelBackend struct{ mountBackend }

func (channelBackend) WriteControlFile(string, []byte, fs.FileMode) error { return nil }
func (channelBackend) RemoveControlFile(string) error                     { return nil }

// execBackend answers every in-instance command with the PRESENT marker, so
// each configure step's probe finds what it looks for and its write succeeds.
type execBackend struct {
	mountBackend
	mu   sync.Mutex
	runs int
}

func (b *execBackend) ExecCommand(string, []string) *backend.Cmd {
	b.mu.Lock()
	b.runs++
	b.mu.Unlock()
	return backend.NewCmd(exec.Command("echo", "PRESENT"), nil)
}

func (b *execBackend) execs() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs
}

func seedSettings(t *testing.T, settings fleet.FleetSettings) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: settings},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func TestMountsHostControlDir(t *testing.T) {
	tests := []struct {
		name string
		b    backend.Backend
		want bool
	}{
		{"backend that binds host dirs", mountBackend{shared: true}, true},
		{"backend without mounts", mountBackend{}, false},
		{"backend with its own control channel", channelBackend{mountBackend{shared: true}}, false},
		{"control channel and no mounts", channelBackend{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mountsHostControlDir(tt.b); got != tt.want {
				t.Fatalf("mountsHostControlDir = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveProvisionMountsGates covers the three gates the mounts go through:
// the fleet's shared mounts need SupportsSharedMounts, the host control
// directory also needs the backend to have no control channel of its own (and
// then is not even created, nor its sockets opened), and the buildkit server's
// mounts come from the instance's shared-service runtime.
func TestResolveProvisionMountsGates(t *testing.T) {
	buildkitMount := backend.Mount{LocalPath: "/host/buildkit", ContainerPath: "/run/buildkit"}
	tests := []struct {
		name         string
		b            backend.Backend
		withServices bool
		wantShared   bool
		wantControl  bool
		wantBuildkit bool
	}{
		{name: "devcontainer-style", b: mountBackend{shared: true}, withServices: true, wantShared: true, wantControl: true, wantBuildkit: true},
		{name: "no shared-service runtime", b: mountBackend{shared: true}, wantShared: true, wantControl: true},
		{name: "own control channel", b: channelBackend{mountBackend{shared: true}}, withServices: true, wantShared: true, wantBuildkit: true},
		{name: "cloud backend", b: mountBackend{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			seedSettings(t, fleet.FleetSettings{ClaudeCodeMount: true, BuildkitServer: true})
			var services svcruntime.Runtime
			rec := svcruntimetest.New("docker")
			rec.Mounts = map[fleet.SharedService][]backend.Mount{fleet.ServiceBuildkit: {buildkitMount}}
			if tt.withServices {
				services = rec
			}
			var readied []string
			orig := ControlDirReady
			ControlDirReady = func(f, i string) { readied = append(readied, f+"/"+i) }
			t.Cleanup(func() { ControlDirReady = orig })

			resolved, err := resolveProvisionMounts(context.Background(), tt.b, services, "alpha", "i1")
			if err != nil {
				t.Fatalf("resolveProvisionMounts: %v", err)
			}

			var shared, controlled, buildkit bool
			for _, m := range resolved.Mounts {
				switch {
				case m.Scope == backend.MountScopeFleetShared:
					shared = true
				case m.ContainerPath == control.ContainerMountDir:
					controlled = true
				case m == buildkitMount:
					buildkit = true
				default:
					t.Errorf("unexpected mount %+v", m)
				}
			}
			if shared != tt.wantShared || controlled != tt.wantControl || buildkit != tt.wantBuildkit {
				t.Fatalf("mounts shared=%v control=%v buildkit=%v, want %v %v %v (%+v)",
					shared, controlled, buildkit, tt.wantShared, tt.wantControl, tt.wantBuildkit, resolved.Mounts)
			}

			_, statErr := os.Stat(state.ControlDir("alpha", "i1"))
			if made := statErr == nil; made != tt.wantControl {
				t.Errorf("host control dir created = %v, want %v", made, tt.wantControl)
			}
			if wantReadied := tt.wantControl; (len(readied) == 1) != wantReadied {
				t.Errorf("ControlDirReady calls = %v, want one = %v", readied, wantReadied)
			}

			var wantCalls []svcruntimetest.Call
			if tt.withServices {
				wantCalls = []svcruntimetest.Call{
					{Method: "PrepareFleet", Fleet: "alpha"},
					{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceBuildkit},
					{Method: "InstanceMounts", Fleet: "alpha", Service: fleet.ServiceBuildkit},
				}
			}
			if got := rec.Calls(); !reflect.DeepEqual(got, wantCalls) {
				t.Errorf("runtime calls = %+v, want %+v", got, wantCalls)
			}
		})
	}
}

// TestResolveProvisionMountsWarnsWhenBuildkitFails: a buildkit server that
// won't start costs only its mount, with a warning; the run goes on.
func TestResolveProvisionMountsWarnsWhenBuildkitFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSettings(t, fleet.FleetSettings{BuildkitServer: true})
	rec := svcruntimetest.New("docker")
	rec.FailOn("EnsureService", errors.New("no docker"))

	resolved, err := resolveProvisionMounts(context.Background(), mountBackend{shared: true}, rec, "alpha", "i1")
	if err != nil {
		t.Fatalf("resolveProvisionMounts: %v", err)
	}
	if len(resolved.Mounts) != 1 || resolved.Mounts[0].ContainerPath != control.ContainerMountDir {
		t.Fatalf("mounts = %+v, want only the control mount", resolved.Mounts)
	}
	if got, _ := os.ReadFile(state.WarnPath("alpha", "i1")); string(got) != "buildkit server: no docker" {
		t.Fatalf("warning = %q", got)
	}
	want := []svcruntimetest.Call{{Method: "PrepareFleet", Fleet: "alpha"}, {Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceBuildkit}}
	if calls := rec.Calls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v, want the prepare and the failed ensure", calls)
	}
}

// TestResolveProvisionMountsFailsWhenTheFleetCannotBePrepared: an instance
// whose runtime cannot make what the fleet's instances need there (on a
// cluster: its sibling DNS and NetworkPolicies) is not created, and nothing
// else is asked of the runtime.
func TestResolveProvisionMountsFailsWhenTheFleetCannotBePrepared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSettings(t, fleet.FleetSettings{BuildkitServer: true})
	rec := svcruntimetest.New("kubernetes:c1")
	rec.FailOn("PrepareFleet", errors.New("networkpolicies is forbidden"))

	_, err := resolveProvisionMounts(context.Background(), mountBackend{shared: true}, rec, "alpha", "i1")
	if err == nil || !strings.Contains(err.Error(), "networkpolicies is forbidden") {
		t.Fatalf("resolveProvisionMounts = %v, want the prepare's error", err)
	}
	if calls := rec.Calls(); !reflect.DeepEqual(calls, []svcruntimetest.Call{{Method: "PrepareFleet", Fleet: "alpha"}}) {
		t.Fatalf("calls = %+v, want only the prepare", calls)
	}
}

// TestCacheSetupLeavesTheInstanceAloneWithoutAnEndpoint: on a runtime that
// runs a cache but points no instance at it, the cache's server is still
// ensured, the step says why the instance was left alone, and nothing inside
// the instance is touched; buildx is left alone without a word (the buildkit
// server's own step already said why).
func TestCacheSetupLeavesTheInstanceAloneWithoutAnEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSettings(t, fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true})
	for name, run := range map[string]func(svcruntime.Runtime, backend.Backend) error{
		"deb cache": func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupDebCache(rt, b, "alpha", "cid", "/ws")
		},
		"image cache": func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupImageCache(rt, b, "alpha", "i1", "cid", "/ws")
		},
	} {
		rec := svcruntimetest.New("kubernetes:c1")
		rec.Endpoints = nil
		b := &execBackend{mountBackend: mountBackend{shared: true}}
		err := run(rec, b)
		if !errors.Is(err, svcruntime.ErrNoEndpoint) {
			t.Errorf("%s: err = %v, want ErrNoEndpoint", name, err)
		}
		if b.execs() != 0 {
			t.Errorf("%s: %d commands ran in the instance", name, b.execs())
		}
		if calls := rec.Calls(); len(calls) == 0 || calls[0].Method != "EnsureService" {
			t.Errorf("%s: calls = %+v, want the server ensured", name, calls)
		}
	}
	rec := svcruntimetest.New("kubernetes:c1")
	rec.Endpoints = nil
	b := &execBackend{mountBackend: mountBackend{shared: true}}
	if err := configureBuildkit(rec, b, "alpha", "/ws"); err != nil || b.execs() != 0 {
		t.Fatalf("configureBuildkit = %v with %d commands, want nothing done and no error", err, b.execs())
	}
}

// TestSharedServiceStepsNeedARuntimeAndTheSetting: each service step is a
// no-op, touching neither the runtime nor the instance, when the instance has
// no shared-service runtime or the fleet has the service off.
func TestSharedServiceStepsNeedARuntimeAndTheSetting(t *testing.T) {
	steps := map[string]func(svcruntime.Runtime, backend.Backend) error{
		"buildkit mounts": func(rt svcruntime.Runtime, _ backend.Backend) error {
			mounts, err := prepareBuildkitMounts(rt, "alpha")
			if len(mounts) != 0 {
				t.Errorf("buildkit mounts without the feature: %+v", mounts)
			}
			return err
		},
		"buildkit buildx": func(rt svcruntime.Runtime, b backend.Backend) error { return configureBuildkit(rt, b, "alpha", "/ws") },
		"deb cache": func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupDebCache(rt, b, "alpha", "cid", "/ws")
		},
		"image cache": func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupImageCache(rt, b, "alpha", "i1", "cid", "/ws")
		},
	}
	all := fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true}
	for name, step := range steps {
		t.Run(name+"/no runtime", func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			seedSettings(t, all)
			if err := step(nil, mountBackend{shared: true}); err != nil {
				t.Fatal(err)
			}
		})
		t.Run(name+"/setting off", func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			seedSettings(t, fleet.FleetSettings{})
			rec := svcruntimetest.New("docker")
			if err := step(rec, mountBackend{shared: true}); err != nil {
				t.Fatal(err)
			}
			if calls := rec.Calls(); len(calls) != 0 {
				t.Fatalf("runtime calls = %+v, want none", calls)
			}
		})
	}
}

// TestCacheSetupEnsuresConnectsThenConfigures: with the service on, a cache
// step ensures its server, connects the instance by container id, then
// configures the instance; a failed ensure or connect stops it there.
func TestCacheSetupEnsuresConnectsThenConfigures(t *testing.T) {
	steps := []struct {
		svc  fleet.SharedService
		run  func(svcruntime.Runtime, backend.Backend) error
		mins int // in-instance commands a successful run issues (probe + write)
	}{
		{fleet.ServiceDebCache, func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupDebCache(rt, b, "alpha", "cid", "/ws")
		}, 2},
		{fleet.ServiceImageCache, func(rt svcruntime.Runtime, b backend.Backend) error {
			return setupImageCache(rt, b, "alpha", "i1", "cid", "/ws")
		}, 2},
	}
	for _, step := range steps {
		t.Run(string(step.svc), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			seedSettings(t, fleet.FleetSettings{DebCacheServer: true, ImageCacheServer: true})

			rec := svcruntimetest.New("docker")
			b := &execBackend{mountBackend: mountBackend{shared: true}}
			if err := step.run(rec, b); err != nil {
				t.Fatalf("setup: %v", err)
			}
			want := []svcruntimetest.Call{
				{Method: "EnsureService", Fleet: "alpha", Service: step.svc},
				{Method: "ConnectInstance", Fleet: "alpha", ContainerID: "cid"},
			}
			if got := rec.Calls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %+v, want %+v", got, want)
			}
			if b.execs() < step.mins {
				t.Fatalf("instance commands = %d, want the probe and the write", b.execs())
			}

			for _, failing := range []string{"EnsureService", "ConnectInstance"} {
				rec := svcruntimetest.New("docker")
				rec.FailOn(failing, errors.New("boom"))
				b := &execBackend{mountBackend: mountBackend{shared: true}}
				if err := step.run(rec, b); err == nil {
					t.Fatalf("%s failed but the setup did not", failing)
				}
				if b.execs() != 0 {
					t.Fatalf("instance configured after %s failed", failing)
				}
			}
		})
	}
}

// TestConfigureBuildkitPointsBuildxAtTheServer: with buildkit on, the instance's
// buildx is configured (probe + write) and the runtime is not asked anything
// more (the server was ensured with the mounts, before Up).
func TestConfigureBuildkitPointsBuildxAtTheServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSettings(t, fleet.FleetSettings{BuildkitServer: true})
	rec := svcruntimetest.New("docker")
	b := &execBackend{mountBackend: mountBackend{shared: true}}

	if err := configureBuildkit(rec, b, "alpha", "/ws"); err != nil {
		t.Fatal(err)
	}
	if b.execs() != 2 {
		t.Fatalf("instance commands = %d, want the probe and the write", b.execs())
	}
	if calls := rec.Calls(); len(calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none", calls)
	}
}

// TestProvisioningJobsAskForTheRecordsRuntime: create, clone and rebuild each
// look up the shared-service runtime for the record they bind their backend
// to.
func TestProvisioningJobsAskForTheRecordsRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t,
		&fleet.Instance{Name: "fresh", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer},
		&fleet.Instance{Name: "src", Status: fleet.StatusRunning, ContainerID: "c1", WorkspaceDir: t.TempDir()},
		&fleet.Instance{Name: "copy", Status: fleet.StatusCloning},
		&fleet.Instance{Name: "busy", Status: fleet.StatusRebuilding, ContainerID: "c2"},
	)
	stubInstanceBackend(t, &fakeJobBackend{upErr: errors.New("stop here")})
	var asked []string
	orig := sharedServicesFor
	sharedServicesFor = func(inst *fleet.Instance) svcruntime.Runtime {
		asked = append(asked, inst.Name)
		return nil
	}
	t.Cleanup(func() { sharedServicesFor = orig })

	_ = RunContext(context.Background(), "alpha", "fresh", "file:///nonexistent-template", "", false, fleet.BackendDevcontainer)
	_ = RunClone(context.Background(), "alpha", "src", "copy", false)
	_ = RunRebuild(context.Background(), "alpha", "busy", false)

	if want := []string{"fresh", "copy", "busy"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("runtime asked for %v, want %v", asked, want)
	}
}
