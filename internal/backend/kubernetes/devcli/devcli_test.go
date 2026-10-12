package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// syncBuffer is a job log several goroutines write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// appWorkspace is a workspace with a Dockerfile build whose context is the
// workspace root.
func appWorkspace(t *testing.T, files map[string]string) (ws, configFile string) {
	t.Helper()
	ws = t.TempDir()
	for name, content := range files {
		path := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws, filepath.Join(ws, ".devcontainer", "devcontainer.json")
}

func TestReadMerged(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"build":{"dockerfile":"Dockerfile","context":".."}`)
	cfg, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigFile != configFile || cfg.WorkspaceFolder != "/workspaces/app" || cfg.CLIVersion != "0.89.0" {
		t.Errorf("config %+v", cfg)
	}
	if string(cfg.Merged) != `{"remoteUser":"vscode"}` {
		t.Errorf("merged %s", cfg.Merged)
	}
	want := &Build{Dockerfile: filepath.Join(ws, ".devcontainer", "Dockerfile"), Context: ws}
	if !reflect.DeepEqual(cfg.Build, want) {
		t.Errorf("build %+v, want %+v", cfg.Build, want)
	}
	args := f.args(t)
	if args[0] != "read-configuration" || !contains(args, "--include-merged-configuration") || !contains(args, "--docker-compose-path") {
		t.Errorf("CLI args %q", args)
	}
	// The CLI's docker calls went through the shim: ps answered empty, the
	// image inspect reached the real docker.
	if log := f.read(t, "docker.log"); !strings.Contains(log, "inspect --type image base:1") || strings.Contains(log, "ps ") {
		t.Errorf("real docker saw %q", log)
	}
}

func TestBuildOf(t *testing.T) {
	cfg := "/ws/.devcontainer/devcontainer.json"
	for _, tc := range []struct {
		members string
		want    *Build
	}{
		{`"image":"x"`, nil},
		{`"build":{"dockerfile":"Dockerfile"}`, &Build{Dockerfile: "/ws/.devcontainer/Dockerfile", Context: "/ws/.devcontainer"}},
		{`"build":{"dockerfile":"../docker/Dockerfile","context":".."}`, &Build{Dockerfile: "/ws/docker/Dockerfile", Context: "/ws"}},
		{`"dockerFile":"Dockerfile","context":"/abs/ctx"`, &Build{Dockerfile: "/ws/.devcontainer/Dockerfile", Context: "/abs/ctx"}},
	} {
		var f configFields
		if err := json.Unmarshal([]byte(`{"configFilePath":{"fsPath":"`+cfg+`"},`+tc.members+`}`), &f); err != nil {
			t.Fatal(err)
		}
		if got := buildOf(f); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.members, got, tc.want)
		}
	}
}

func TestReadMergedRefusesComposeBeforeAnyBuild(t *testing.T) {
	for _, members := range []string{`,"dockerComposeFile":"docker-compose.yml","service":"app"`, `,"dockerComposeFile":[]`} {
		f := newFakeTools(t)
		ws, configFile := appWorkspace(t, nil)
		f.writeConfigs(t, configFile, members)
		_, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
		if !IsCompose(err) || !strings.HasPrefix(err.Error(), "kubernetes:unsupported:compose: ") || !strings.Contains(err.Error(), configFile) {
			t.Fatalf("%s: err = %v", members, err)
		}
		if contains(f.args(t), "--include-merged-configuration") {
			t.Errorf("the merged read (which may build) ran for a compose configuration")
		}
		if _, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws}); !IsCompose(err) {
			t.Errorf("CaptureUp without a preflight: %v", err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "run.rc")); err == nil {
			t.Errorf("a compose configuration reached docker run")
		}
	}
}

// TestReadMergedComposeBackstop: a compose call that reaches the shim fails
// the read as the compose refusal, whatever the plain read said.
func TestReadMergedComposeBackstop(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	t.Setenv("FAKE_MERGED_COMPOSE", "1")
	_, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
	if !IsCompose(err) {
		t.Fatalf("err = %v, want the compose refusal", err)
	}
}

func TestCaptureUp(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, map[string]string{
		".devcontainer/Dockerfile": "FROM base:1\n",
		".env":                     "TOKEN=x\n",
		"certs/server.pem":         "-----BEGIN-----\n",
		"README.md":                "hi\n",
	})
	f.writeConfigs(t, configFile, `,"build":{"dockerfile":"Dockerfile","context":".."}`)
	jobLog := &syncBuffer{}
	ctx := backend.WithJobLog(context.Background(), jobLog)
	t.Setenv("FLEET_DEVCONTAINER_BUILDKIT", "")
	res, err := CaptureUp(ctx, CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, GPU: true, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if rc := strings.TrimSpace(f.read(t, "run.rc")); rc != "97" {
		t.Errorf("the shim's run exited %s, want 97", rc)
	}
	if res.Run.Image != "vsc-app-1234" || res.Image.ID != "sha256:built" || res.CLIVersion != "0.89.0" || res.FromRegistry {
		t.Errorf("run %q image %q cli %q from its registry %v", res.Run.Image, res.Image.ID, res.CLIVersion, res.FromRegistry)
	}
	if len(res.Capture.Env) != 1 || res.Capture.Env[0] != (kubeshim.EnvVar{Name: "SECRET_TOKEN", Value: "hunter2", Source: "flag"}) {
		t.Errorf("env %+v", res.Capture.Env)
	}
	if res.Container.Labels["image.label"] != "1" || res.Container.Labels["devcontainer.local_folder"] != ws {
		t.Errorf("labels %v", res.Container.Labels)
	}
	if res.Container.User != "vscode" || res.RemoteUser != "vscode" {
		t.Errorf("user %q remote user %q", res.Container.User, res.RemoteUser)
	}
	args := f.args(t)
	for _, want := range [][]string{
		{"up", "--workspace-folder", ws},
		{"--update-remote-user-uid-default", "never"},
		{"--gpu-availability", "all"},
		{"--build-no-cache"},
	} {
		if !containsRun(args, want) {
			t.Errorf("CLI args %q lack %q", args, want)
		}
	}
	env := f.read(t, "cli.env")
	if !strings.Contains(env, "BUILDX_BUILDER=desktop-linux\n") || strings.Contains(env, "DOCKER_DEFAULT_PLATFORM") || strings.Contains(env, "DOCKER_BUILDKIT=0") {
		t.Errorf("CLI env: builder from `docker context show`, no platform, buildkit untouched:\n%s", env)
	}
	if !strings.Contains(env, "TMPDIR=") || strings.Contains(env, "TMPDIR="+os.TempDir()+"\n") {
		t.Errorf("the CLI needs its own TMPDIR")
	}

	// Both secret warnings, reported to the job log too.
	if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "no .dockerignore") || !strings.Contains(res.Warnings[1], ".env, certs/server.pem") {
		t.Errorf("warnings %q", res.Warnings)
	}
	log := jobLog.String()
	if !strings.Contains(log, "warning: the image build context") || !strings.Contains(log, "@devcontainers/cli 0.89.0") {
		t.Errorf("job log:\n%s", log)
	}
	if !strings.Contains(log, "captured: docker run") || strings.Contains(strings.SplitN(log, "captured:", 2)[1], "hunter2") {
		t.Errorf("the job log's capture line must be redacted:\n%s", log)
	}

	var verbs []string
	for _, c := range res.Calls {
		verbs = append(verbs, c.Verb()+":"+string(c.Outcome))
		if strings.Contains(strings.Join(c.Args, " "), "hunter2") {
			t.Errorf("the journal holds an env value: %v", c.Args)
		}
	}
	for _, want := range []string{"buildx version:pass", "ps:empty", "events:block", "run:captured"} {
		if !contains(verbs, want) {
			t.Errorf("calls %v lack %s", verbs, want)
		}
	}
}

func TestCaptureUpEnvironment(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, map[string]string{".devcontainer/.dockerignore": "x\n"})
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	cfg, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_DEVCONTAINER_BUILDKIT", "never")
	res, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Config: cfg, Builder: "pinned", Platform: "linux/" + hostArch()})
	if err != nil {
		t.Fatal(err)
	}
	env := f.read(t, "cli.env")
	for _, want := range []string{"BUILDX_BUILDER=pinned\n", "DOCKER_DEFAULT_PLATFORM=linux/" + hostArch() + "\n", "DOCKER_BUILDKIT=0\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("CLI env lacks %q", want)
		}
	}
	if !containsRun(f.args(t), []string{"--gpu-availability", "none"}) || contains(f.args(t), "--build-no-cache") {
		t.Errorf("CLI args %q", f.args(t))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("an image-only configuration got warnings: %q", res.Warnings)
	}
	if strings.Contains(f.read(t, "docker.log"), "context show") {
		t.Errorf("a pinned builder needs no docker context show")
	}

	other := "arm64"
	if hostArch() == "arm64" {
		other = "amd64"
	}
	_, err = CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Config: cfg, Platform: "linux/" + other})
	if err == nil || !strings.Contains(err.Error(), "FLEET_DEVCONTAINER_BUILDKIT=never") {
		t.Errorf("a cross-arch build with BuildKit off: %v", err)
	}
	t.Setenv("FLEET_DEVCONTAINER_BUILDKIT", "sometimes")
	if _, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Config: cfg}); err == nil || !strings.Contains(err.Error(), "invalid FLEET_DEVCONTAINER_BUILDKIT") {
		t.Errorf("an invalid FLEET_DEVCONTAINER_BUILDKIT: %v", err)
	}
}

// TestCaptureUpBindsTheOriginAgent: a configuration whose
// ${localEnv:SSH_AUTH_SOCK} read as the daemon's relay socket is captured
// with the agent the daemon was started with, as the devcontainer backend's
// up does; one that does not name the relay keeps fleetd's environment.
func TestCaptureUpBindsTheOriginAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := os.MkdirTemp("/tmp", "dca")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	origin := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", origin)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	relay := agentsock.HostSocketPath()
	t.Setenv(agentsock.EnvAuthSock, relay)
	t.Setenv(agentsock.EnvOrigin, origin)

	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, map[string]string{".devcontainer/.dockerignore": "x\n"})
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	cfg, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, config, want string
	}{
		{"names the relay", `{"mounts":["source=` + relay + `,target=/ssh.sock,type=bind"]}`, origin},
		{"does not", `{"image":"base:1"}`, relay},
	} {
		c := *cfg
		c.Configuration = json.RawMessage(tc.config)
		if _, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Config: &c, Builder: "pinned"}); err != nil {
			t.Fatal(err)
		}
		env := "\n" + f.read(t, "cli.env")
		if !strings.Contains(env, "\n"+agentsock.EnvAuthSock+"="+tc.want+"\n") {
			t.Errorf("%s: CLI env lacks SSH_AUTH_SOCK=%s:\n%s", tc.name, tc.want, env)
		}
	}
}

// TestCaptureUpPullsAnImageTheHostLacks: an image-only configuration's image
// is never built, and the captured run would have pulled it, so capture-up
// pulls it to read its config when its registry cannot be read.
func TestCaptureUpPullsAnImageTheHostLacks(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	if err := os.Rename(filepath.Join(f.dir, "image.json"), filepath.Join(f.dir, "image.remote.json")); err != nil {
		t.Fatal(err)
	}
	jobLog := &syncBuffer{}
	// The image's registry cannot say (offline): capture-up pulls.
	res, err := CaptureUp(backend.WithJobLog(context.Background(), jobLog), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Platform: "linux/arm64", Remote: offlineRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID != "sha256:built" {
		t.Errorf("image %+v", res.Image)
	}
	if log := f.read(t, "docker.log"); !strings.Contains(log, "pull --platform linux/arm64 -- vsc-app-1234") {
		t.Errorf("real docker saw:\n%s", log)
	}
	if !strings.Contains(jobLog.String(), "Downloaded newer image") {
		t.Errorf("the pull's output is not in the job log:\n%s", jobLog.String())
	}

	// A failed pull says why the image could not be read.
	_ = os.Remove(filepath.Join(f.dir, "image.json"))
	_ = os.Remove(filepath.Join(f.dir, "image.remote.json"))
	_, err = CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Remote: offlineRegistry()})
	if err == nil || !strings.Contains(err.Error(), "No such image") || !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("a missing image that does not pull: %v", err)
	}
}

// TestCaptureUpReportsTheIDDockerAddressesTheImageBy: on a containerd image
// store a platform's inspect names that platform's manifest, which docker
// cannot tag or save; the image's own id (its index's) is what delivery
// takes, the config the platform's.
func TestCaptureUpReportsTheIDDockerAddressesTheImageBy(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	if err := os.WriteFile(filepath.Join(f.dir, "image.platform.json"), []byte(`[{"Id":"sha256:arm64manifest","Architecture":"arm64","Os":"linux","Config":{"User":"arm-user"}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Platform: "linux/arm64", Remote: offlineRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID != "sha256:built" || res.Image.Architecture != "arm64" || res.Image.Config.User != "arm-user" {
		t.Errorf("image %+v", res.Image)
	}
	// Without a platform there is one inspect, and its id.
	res, err = CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Remote: offlineRegistry()})
	if err != nil || res.Image.ID != "sha256:built" || res.Image.Architecture != "amd64" {
		t.Errorf("without a platform: %+v, %v", res.Image, err)
	}
}

func TestCaptureUpWarnsAboutAnUntestedCLI(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	t.Setenv("FAKE_CLI_VERSION", "9.9.9")
	res, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "devcontainer CLI 9.9.9 is not one fleet has tested") {
		t.Errorf("warnings %q", res.Warnings)
	}
}

func TestCaptureUpFailsBeforeTheRun(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	t.Setenv("FAKE_UP_FAIL", "the Dockerfile has no FROM -e LEAK=hunter2")
	_, err := CaptureUp(context.Background(), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err == nil {
		t.Fatal("CaptureUp succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"devcontainer up (image build) failed", "An error occurred building the image.", "the Dockerfile has no FROM", "the docker shim refused: `docker info`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "hunter2") {
		t.Errorf("the error holds an env value:\n%s", msg)
	}
}

func TestCaptureUpCancelled(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"base:1"`)
	cfg, err := ReadMerged(context.Background(), ReadRequest{Tools: f.Tools, WorkspaceDir: ws})
	if err != nil {
		t.Fatal(err)
	}
	slow := filepath.Join(f.dir, "slow-devcontainer")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := f.Tools
	tools.CLI = slow
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = CaptureUp(ctx, CaptureRequest{Tools: tools, WorkspaceDir: ws, Config: cfg})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("a cancelled capture-up: %v after %v", err, time.Since(start))
	}
}

// recordingExecer records each exec and answers like a pod would.
type recordingExecer struct {
	mu    sync.Mutex
	reqs  []kubeshim.ExecRequest
	stdin []string
	// fail makes the exec whose argv contains it exit 1.
	fail string
}

func (r *recordingExecer) Exec(ctx context.Context, req kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	var in []byte
	if stdio.Stdin != nil {
		in, _ = io.ReadAll(stdio.Stdin)
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.stdin = append(r.stdin, string(in))
	r.mu.Unlock()
	if r.fail != "" && strings.Contains(strings.Join(req.Argv, " "), r.fail) {
		_, _ = stdio.Stderr.Write([]byte("permission denied"))
		return 1, nil
	}
	_, _ = stdio.Stdout.Write([]byte("hook ran as " + req.User))
	return 0, nil
}

func setUpContainer() kubeshim.Container {
	return kubeshim.Container{
		ContainerID: "k8s:c1/fleet/fi-app-main-1a2b3c4d@g1",
		Name:        "fi-app-main-1a2b3c4d-0",
		Image:       "vsc-app-1234",
		Created:     time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		StartedAt:   time.Date(2026, 10, 2, 8, 0, 0, 123, time.UTC),
		User:        "root",
		Labels:      map[string]string{"devcontainer.metadata": `[{"remoteUser":"vscode"}]`},
	}
}

func TestSetUp(t *testing.T) {
	f := newFakeTools(t)
	ex := &recordingExecer{}
	jobLog := &syncBuffer{}
	ctx := backend.WithJobLog(context.Background(), jobLog)
	c := setUpContainer()
	res, err := SetUp(ctx, SetUpRequest{Tools: f.Tools, Container: c, WorkspaceFolder: "/workspaces/app", Execer: ex,
		RemoteEnv: []string{"SSH_AUTH_SOCK=/fleet-mounts/control/ssh-agent.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Merged) != `{"remoteUser":"vscode"}` || res.CLIVersion != "0.89.0" {
		t.Errorf("result %+v", res)
	}
	if len(ex.reqs) != 3 {
		t.Fatalf("execs %+v, want the data folder's two and the hook", ex.reqs)
	}
	mkdir, check, hook := ex.reqs[0], ex.reqs[1], ex.reqs[2]
	if mkdir.User != "root" || !reflect.DeepEqual(mkdir.Argv[3:], []string{"sh", DefaultDataFolder, "vscode"}) || !strings.Contains(mkdir.Argv[2], "chown") {
		t.Errorf("data folder exec %+v", mkdir)
	}
	if check.User != "vscode" || !strings.Contains(check.Argv[2], "test -w") {
		t.Errorf("writability check %+v", check)
	}
	wantHook := kubeshim.ExecRequest{User: "vscode", Env: []string{"HOOK=postCreate"}, Dir: "/workspaces/app", Argv: []string{"/bin/sh", "-c", "run hook"}, Stdin: true}
	if !reflect.DeepEqual(hook, wantHook) || ex.stdin[2] != "from stdin\n" {
		t.Errorf("hook exec %+v stdin %q, want %+v", hook, ex.stdin[2], wantHook)
	}
	if out := f.read(t, "exec.out"); out != "hook ran as vscode" {
		t.Errorf("the CLI read %q from its exec", out)
	}
	var inspect []map[string]any
	if err := json.Unmarshal([]byte(f.read(t, "inspect.json")), &inspect); err != nil || len(inspect) != 1 || inspect[0]["Created"] != "2026-10-01T12:00:00Z" {
		t.Errorf("the CLI's inspect: %v %v", inspect, err)
	}
	if got := f.read(t, "setup.config"); got != `{"workspaceFolder":"/workspaces/app"}` {
		t.Errorf("set-up's configuration %s", got)
	}
	args := f.args(t)
	for _, want := range [][]string{
		{"set-up", "--container-id", c.ContainerID},
		{"--container-data-folder", DefaultDataFolder},
		{"--include-merged-configuration"},
		{"--remote-env", "SSH_AUTH_SOCK=/fleet-mounts/control/ssh-agent.sock"},
	} {
		if !containsRun(args, want) {
			t.Errorf("CLI args %q lack %q", args, want)
		}
	}
	if contains(args, "--skip-post-create") {
		t.Errorf("--skip-post-create without asking")
	}
	for _, call := range res.Calls {
		if call.Outcome != kubeshim.OutcomeInspect && call.Outcome != kubeshim.OutcomeExec && call.Outcome != kubeshim.OutcomeNoEngine {
			t.Errorf("pod mode call %+v", call)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(f.dir, "docker.log")); len(log) != 0 {
		t.Errorf("set-up reached the real docker: %s", log)
	}
}

func TestSetUpFailures(t *testing.T) {
	f := newFakeTools(t)
	c := setUpContainer()
	ctx := context.Background()

	_, err := SetUp(ctx, SetUpRequest{Tools: f.Tools, Container: c, Execer: &recordingExecer{fail: "test -w"}, DataFolder: "/data"})
	if err == nil || !strings.Contains(err.Error(), "/data is not writable by vscode, so set-up would skip the lifecycle hooks") {
		t.Errorf("an unwritable data folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "cli.args")); err == nil {
		t.Errorf("set-up ran with an unwritable data folder")
	}

	_, err = SetUp(ctx, SetUpRequest{Tools: f.Tools, Container: c, Execer: &recordingExecer{fail: "run hook"}, SkipPostCreate: true})
	if err == nil || !strings.Contains(err.Error(), "postCreateCommand from devcontainer.json failed") {
		t.Errorf("a failed hook: %v", err)
	}
	if !contains(f.args(t), "--skip-post-create") {
		t.Errorf("SkipPostCreate not passed: %q", f.args(t))
	}
	if got := f.read(t, "setup.config"); got != `{}` {
		t.Errorf("no workspace folder: set-up's configuration %s", got)
	}

	if _, err := SetUp(ctx, SetUpRequest{Tools: f.Tools, Execer: &recordingExecer{}}); err == nil {
		t.Errorf("SetUp without a container")
	}
	noStart := c
	noStart.StartedAt = time.Time{}
	if _, err := SetUp(ctx, SetUpRequest{Tools: f.Tools, Container: noStart, Execer: &recordingExecer{}}); err == nil || !strings.Contains(err.Error(), "StartedAt") {
		t.Errorf("SetUp without a StartedAt: %v", err)
	}
}

func TestTestedCLIVersionsHaveSignatures(t *testing.T) {
	recorded := map[string]bool{}
	files, err := filepath.Glob(filepath.Join("testdata", "signature", "devcontainer-cli-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "devcontainer-cli-"), ".json")
		recorded[v] = true
		var sig signatureFile
		data, _ := os.ReadFile(file)
		if err := json.Unmarshal(data, &sig); err != nil || sig.CLI != v || len(sig.Fixtures) != len(signatureFixtures) {
			t.Errorf("%s: cli %q, %d fixtures, %v", file, sig.CLI, len(sig.Fixtures), err)
		}
		if !Tested(v) {
			t.Errorf("%s is recorded but not in TestedCLIVersions", v)
		}
	}
	for _, v := range TestedCLIVersions {
		if !recorded[v] {
			t.Errorf("TestedCLIVersions has %s, which has no signature in testdata/signature", v)
		}
	}
}

func TestCLIVersionFromTheLog(t *testing.T) {
	if v := cliVersion("[2026-10-10T09:55:06.373Z] @devcontainers/cli 0.89.0. Node.js v24.21.0. linux"); v != "0.89.0" {
		t.Errorf("cliVersion = %q", v)
	}
	if v := cliVersion("no version here"); v != "" {
		t.Errorf("cliVersion = %q", v)
	}
}

// containsRun reports whether want appears in args as a contiguous run.
func containsRun(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func hostArch() string { return runtime.GOARCH }

// TestToolsDocker: the real docker is handed to the shim as an absolute
// path, a bare name found on PATH as docker would be.
func TestToolsDocker(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"docker", "other-docker"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"":              filepath.Join(bin, "docker"),
		"other-docker":  filepath.Join(bin, "other-docker"),
		"./rel/docker":  filepath.Join(cwd, "rel", "docker"),
		"/abs/x/docker": "/abs/x/docker",
	} {
		got, err := Tools{Docker: in}.docker()
		if err != nil || got != want {
			t.Errorf("Docker %q: %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := (Tools{Docker: "missing-docker"}).docker(); err == nil {
		t.Errorf("a docker name not on PATH should fail")
	}
}
