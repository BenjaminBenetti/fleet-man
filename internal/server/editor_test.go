package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// editorFake is an instance backend for the editor paths: its editor URI,
// its editor info, and the fleet binary staging it was asked for.
type editorFake struct {
	backend.Backend
	uri     string
	info    backend.EditorInfo
	infoErr error

	mu     sync.Mutex
	staged int
}

func (f *editorFake) EditorURI(string, string) (string, bool) { return f.uri, f.uri != "" }

func (f *editorFake) EditorInfo(context.Context, string) (backend.EditorInfo, error) {
	return f.info, f.infoErr
}

func (f *editorFake) StageFleet(string, func() error) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staged++
	return false, nil
}

// uriOnly is a backend with an editor URI and no Remote-SSH path.
type uriOnly struct {
	backend.Backend
	uri string
}

func (u uriOnly) EditorURI(string, string) (string, bool) { return u.uri, true }

func stubEditorBackend(t *testing.T, b func(*fleet.Instance) backend.Backend) {
	t.Helper()
	orig := editorBackend
	editorBackend = b
	t.Cleanup(func() { editorBackend = orig })
}

func seedEditorInstances(t *testing.T, insts ...*fleet.Instance) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: insts}}}); err != nil {
		t.Fatal(err)
	}
}

// editorClients are the local socket's client and a token-gated listener's.
func editorClients(t *testing.T, svc *service) (local fleetgrpc.FleetServiceClient, remote fleetgrpc.FleetServiceClient, remoteCtx context.Context) {
	t.Helper()
	gsLocal := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gsLocal, svc)
	local = dialBuf(t, gsLocal)
	const token = "editor-token"
	authUnary, authStream := bearerAuthInterceptors(token)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	remote = dialBuf(t, gs)
	return local, remote, metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

// The daemon picks how a caller opens VS Code: the Dev Containers URI only
// for a local caller of a devcontainer instance; Remote-SSH for a remote
// caller and for a cluster instance; the Coder and Codespaces CLIs as before.
func TestResolveEditorLaunchPicksTheMode(t *testing.T) {
	isolateFleetDir(t)
	devURI := "vscode-remote://dev-container+2f7773/workspaces/alpha"
	seedEditorInstances(t,
		&fleet.Instance{Name: "dev", Backend: fleet.BackendDevcontainer, WorkspaceDir: "/ws", ContainerID: "c1", Status: fleet.StatusRunning},
		&fleet.Instance{Name: "pod", Backend: fleet.BackendKubernetes, ContainerID: "k8s:x/ns/sts@g1", Status: fleet.StatusRunning},
		&fleet.Instance{Name: "coder", Backend: fleet.BackendCoder, ContainerID: "ws-1", Status: fleet.StatusRunning},
		&fleet.Instance{Name: "cs", Backend: fleet.BackendCodespaces, ContainerID: "cs-1", Status: fleet.StatusRunning},
		&fleet.Instance{Name: "stopped", Backend: fleet.BackendKubernetes, Status: fleet.StatusStopped},
		&fleet.Instance{Name: "uri-only", Backend: fleet.BackendDevcontainer, Status: fleet.StatusRunning},
	)
	stubEditorBackend(t, func(inst *fleet.Instance) backend.Backend {
		switch inst.Name {
		case "dev":
			return &editorFake{uri: devURI, info: backend.EditorInfo{WorkspaceFolder: "/workspaces/alpha"}}
		case "uri-only":
			return uriOnly{uri: devURI}
		default:
			return &editorFake{uri: "vscode-remote://ssh-remote+pod.alpha.fleet/workspaces/repo", info: backend.EditorInfo{WorkspaceFolder: "/workspaces/repo"}}
		}
	})
	local, remote, remoteCtx := editorClients(t, newService())

	type want struct {
		mode   fleetgrpc.EditorLaunchMode
		argv   []string
		folder string
		code   codes.Code
	}
	ssh := fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH
	for _, tc := range []struct {
		inst          string
		local, remote want
	}{
		{"dev",
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI, argv: []string{"code", "--folder-uri", devURI}},
			want{mode: ssh, folder: "/workspaces/alpha"}},
		{"pod", want{mode: ssh, folder: "/workspaces/repo"}, want{mode: ssh, folder: "/workspaces/repo"}},
		{"coder",
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODER_CLI, argv: []string{"coder", "open", "vscode", "ws-1"}},
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODER_CLI, argv: []string{"coder", "open", "vscode", "ws-1"}}},
		{"cs",
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODESPACES_CLI, argv: []string{"gh", "codespace", "code", "-c", "cs-1"}},
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODESPACES_CLI, argv: []string{"gh", "codespace", "code", "-c", "cs-1"}}},
		{"stopped", want{code: codes.FailedPrecondition}, want{code: codes.FailedPrecondition}},
		{"uri-only",
			want{mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI, argv: []string{"code", "--folder-uri", devURI}},
			want{code: codes.FailedPrecondition}},
		{"missing", want{code: codes.NotFound}, want{code: codes.NotFound}},
	} {
		for _, via := range []struct {
			name   string
			client fleetgrpc.FleetServiceClient
			ctx    context.Context
			want   want
		}{{"local", local, context.Background(), tc.local}, {"remote", remote, remoteCtx, tc.remote}} {
			reply, err := via.client.ResolveEditorLaunch(via.ctx, &fleetgrpc.ResolveEditorLaunchRequest{Fleet: "alpha", Instance: tc.inst})
			if via.want.code != codes.OK {
				if status.Code(err) != via.want.code {
					t.Errorf("%s over %s: %v, want %s", tc.inst, via.name, err, via.want.code)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s over %s: %v", tc.inst, via.name, err)
				continue
			}
			if reply.GetMode() != via.want.mode || !slices.Equal(reply.GetArgv(), via.want.argv) || reply.GetWorkspaceFolder() != via.want.folder {
				t.Errorf("%s over %s = %v %q %q, want %v %q %q", tc.inst, via.name, reply.GetMode(), reply.GetArgv(), reply.GetWorkspaceFolder(),
					via.want.mode, via.want.argv, via.want.folder)
			}
		}
	}
}

// The endpoint records live on the local daemon only: saved and looked up
// by the alias's remote part, 0600, and refused over a token-gated listener.
func TestEditorEndpointsAreLocalOnly(t *testing.T) {
	isolateFleetDir(t)
	local, remote, remoteCtx := editorClients(t, newService())
	ctx := context.Background()

	gw := &fleetgrpc.EditorEndpoint{Kind: fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY, Address: "https://gw.example/grpc/s1", Token: "tok"}
	saved, err := local.SaveEditorEndpoint(ctx, &fleetgrpc.SaveEditorEndpointRequest{Endpoint: gw})
	if err != nil {
		t.Fatal(err)
	}
	if want := sshalias.RemoteHash("https://gw.example/grpc/s1"); saved.GetRemote() != want {
		t.Fatalf("remote %q, want %q", saved.GetRemote(), want)
	}
	srv := &fleetgrpc.EditorEndpoint{Kind: fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER, Address: "10.0.0.1:7777"}
	savedSrv, err := local.SaveEditorEndpoint(ctx, &fleetgrpc.SaveEditorEndpointRequest{Endpoint: srv})
	if err != nil || savedSrv.GetRemote() != sshalias.RemoteHash("server:10.0.0.1:7777") {
		t.Fatalf("server endpoint: %v %v", savedSrv, err)
	}

	got, err := local.ResolveEditorEndpoint(ctx, &fleetgrpc.ResolveEditorEndpointRequest{Remote: saved.GetRemote()})
	if err != nil || got.GetEndpoint().GetAddress() != gw.Address || got.GetEndpoint().GetToken() != "tok" || got.GetEndpoint().GetKind() != gw.Kind {
		t.Fatalf("resolve: %v %v", got, err)
	}
	none, err := local.ResolveEditorEndpoint(ctx, &fleetgrpc.ResolveEditorEndpointRequest{Remote: "r000000"})
	if err != nil || none.GetEndpoint() != nil {
		t.Fatalf("an unknown remote resolved: %v %v", none, err)
	}
	if _, err := local.ResolveEditorEndpoint(ctx, &fleetgrpc.ResolveEditorEndpointRequest{Remote: "../x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bad remote part: %v", err)
	}
	if _, err := local.SaveEditorEndpoint(ctx, &fleetgrpc.SaveEditorEndpointRequest{Endpoint: &fleetgrpc.EditorEndpoint{Address: "x"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an endpoint with no kind: %v", err)
	}
	if info, err := os.Stat(state.EditorEndpointsPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("editor_endpoints.json: %v %v", info, err)
	}

	if _, err := remote.SaveEditorEndpoint(remoteCtx, &fleetgrpc.SaveEditorEndpointRequest{Endpoint: gw}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SaveEditorEndpoint over the tunnel: %v, want PermissionDenied", err)
	}
	if _, err := remote.ResolveEditorEndpoint(remoteCtx, &fleetgrpc.ResolveEditorEndpointRequest{Remote: saved.GetRemote()}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("ResolveEditorEndpoint over the tunnel: %v, want PermissionDenied", err)
	}
}

// recordingSink is a control directory that keeps what was written to it.
type recordingSink struct {
	mu    sync.Mutex
	files map[string][]byte
	modes map[string]fs.FileMode
}

func (s *recordingSink) key() string   { return "test" }
func (s *recordingSink) reaches() bool { return true }
func (s *recordingSink) write(name string, data []byte, mode fs.FileMode, now time.Time) (controlStamp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = append([]byte(nil), data...)
	s.modes[name] = mode
	return controlStamp{at: now}, nil
}
func (s *recordingSink) intact(string, controlStamp, time.Time) bool { return true }
func (s *recordingSink) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, name)
	return nil
}

// An Exec of the in-instance ssh-server is an editor session: the fleet
// binary is staged, the instance's EditorConfig lands in its control
// directory before the command starts, and ssh-server's report goes to the
// creation log as well as to the caller. Any other Exec does none of that.
func TestEditorSessionWritesTheConfigAndLogs(t *testing.T) {
	isolateFleetDir(t)
	editorStaged = sync.Map{}
	seedExecInstance(t)
	label := `[{"customizations":{"vscode":{"extensions":["a.b"],"settings":{"x":1}}},"postAttachCommand":"echo hi"}]`
	fake := &editorFake{info: backend.EditorInfo{WorkspaceFolder: "/workspaces/alpha", Metadata: label}}
	stubEditorBackend(t, func(*fleet.Instance) backend.Backend { return fake })
	sink := &recordingSink{files: map[string][]byte{}, modes: map[string]fs.FileMode{}}
	origSink := controlSinkFor
	controlSinkFor = func(string, *fleet.Instance) controlSink { return sink }
	t.Cleanup(func() { controlSinkFor = origSink })
	var sawConfig []bool
	origOpen := openExecStream
	openExecStream = func(_ context.Context, _ *fleet.Instance, start *fleetgrpc.ExecStart) (backend.ExecStream, error) {
		sink.mu.Lock()
		sawConfig = append(sawConfig, sink.files[devmeta.EditorConfigName] != nil)
		sink.mu.Unlock()
		return startCmdExecStream(exec.Command("sh", "-c", `printf 'SSH-2.0-x\r\n'; echo "==> vscode extension a.b: installed" >&2; printf 'docker run -e SECRET=hunter2 partial' >&2`), false)
	}
	t.Cleanup(func() { openExecStream = origOpen })

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	run := func(argv []string) (string, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := client.Exec(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(execStartFrame("alpha", "i1", argv, false, nil)); err != nil {
			t.Fatal(err)
		}
		_ = stream.CloseSend()
		stdout, stderr, exit := drainExec(t, stream)
		if exit.GetCode() != 0 {
			t.Fatalf("exit %v", exit)
		}
		return string(stdout), string(stderr)
	}

	stdout, stderr := run(sshalias.ServerArgv())
	if stdout != "SSH-2.0-x\r\n" || !strings.Contains(stderr, "a.b: installed") {
		t.Fatalf("the caller got %q / %q", stdout, stderr)
	}
	if len(sawConfig) != 1 || !sawConfig[0] {
		t.Fatal("the editor settings were not in the control directory before ssh-server started")
	}
	var cfg devmeta.EditorConfig
	if err := json.Unmarshal(sink.files[devmeta.EditorConfigName], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Gen != "c1" || cfg.WorkspaceFolder != "/workspaces/alpha" || !slices.Equal(cfg.Extensions, []string{"a.b"}) || len(cfg.PostAttach) != 1 {
		t.Fatalf("editor config %+v", cfg)
	}
	if sink.modes[devmeta.EditorConfigName] != 0o644 {
		t.Errorf("editor config mode %v", sink.modes[devmeta.EditorConfigName])
	}
	if fake.staged != 1 {
		t.Errorf("the fleet binary was staged %d times", fake.staged)
	}
	log, err := os.ReadFile(state.InstanceCreationLogPath("alpha", "i1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "==> vscode extension a.b: installed\n") {
		t.Errorf("creation log lacks the report:\n%s", log)
	}
	if strings.Contains(string(log), "hunter2") || !strings.Contains(string(log), "SECRET=<redacted>\n") {
		t.Errorf("creation log did not redact or did not end the unfinished line:\n%s", log)
	}
	if strings.Contains(string(log), "SSH-2.0") {
		t.Errorf("the SSH stream reached the creation log:\n%s", log)
	}

	// A second connection does not stage again.
	run(sshalias.ServerArgv())
	if fake.staged != 1 {
		t.Errorf("a second editor session staged again (%d)", fake.staged)
	}

	// Any other command is a plain Exec.
	sink.files = map[string][]byte{}
	before, _ := os.ReadFile(state.InstanceCreationLogPath("alpha", "i1"))
	run([]string{fleetlaunch.RemotePath, "ssh-server"})
	if sink.files[devmeta.EditorConfigName] != nil {
		t.Error("a plain Exec wrote editor settings")
	}
	after, _ := os.ReadFile(state.InstanceCreationLogPath("alpha", "i1"))
	if string(after) != string(before) {
		t.Errorf("a plain Exec wrote to the creation log:\n%s", after)
	}

	// An instance whose settings cannot be read still connects.
	fake.infoErr = errors.New("no label")
	sink.files = map[string][]byte{}
	if stdout, _ := run(sshalias.ServerArgv()); stdout != "SSH-2.0-x\r\n" {
		t.Errorf("a session without settings got %q", stdout)
	}
	if sink.files[devmeta.EditorConfigName] != nil {
		t.Error("settings written without the instance's metadata")
	}
}

// ssh-proxy runs what the daemon recognizes, by the staged binary's path.
func TestEditorSessionArgvIsTheStagedBinary(t *testing.T) {
	if argv := sshalias.ServerArgv(); argv[0] != fleetlaunch.RemotePath || !isEditorSession(argv) {
		t.Fatalf("ServerArgv %q does not run %s", argv, fleetlaunch.RemotePath)
	}
}

// What an editor session appends to the creation log is redacted, cut per
// line and capped per session.
func TestCreationLogAppenderCaps(t *testing.T) {
	isolateFleetDir(t)
	a := newCreationLogAppender("alpha", "i1")
	_, _ = a.Write([]byte("first\r\n" + strings.Repeat("x", creationLogLine+10) + "\n"))
	for range 200 {
		_, _ = a.Write([]byte(strings.Repeat("y", 8<<10) + "\n"))
	}
	_ = a.Close()
	data, err := os.ReadFile(state.InstanceCreationLogPath("alpha", "i1"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.HasPrefix(log, "first\n") || !strings.Contains(log, " [line cut]\n") {
		t.Fatalf("lines: %.200q", log)
	}
	if len(data) > creationLogSession+200 || !strings.HasSuffix(log, "report was cut at 1 MiB\n") {
		t.Fatalf("%d bytes, ends %q", len(data), log[len(log)-60:])
	}
	if info, _ := os.Stat(state.InstanceCreationLogPath("alpha", "i1")); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}
