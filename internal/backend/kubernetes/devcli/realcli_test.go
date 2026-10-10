package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// The tests in this file run the real devcontainer CLI against the real
// docker on this host: they build images and, for the set-up replay, run a
// container that stands in for the pod. They run only with
// FLEET_KUBESHIM_CLI=1 (the kubeshim-cli CI job sets it, once per tested CLI
// version); FLEET_KUBESHIM_UPDATE=1 records the capture signature of the
// installed CLI instead of checking it.

// requireCLI skips unless the real-CLI tests were asked for, and fails when
// they were but a tool is missing.
func requireCLI(t *testing.T) {
	t.Helper()
	if os.Getenv("FLEET_KUBESHIM_CLI") != "1" {
		t.Skip("set FLEET_KUBESHIM_CLI=1 to run against the real devcontainer CLI and docker")
	}
	for _, tool := range []string{"devcontainer", "docker", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("FLEET_KUBESHIM_CLI=1 needs %s on PATH", tool)
		}
	}
}

// repoRoot is the module root, four levels up.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// workspace copies a fixture into a fresh git checkout, as fleet's clone
// would leave it, and returns its path. Mode 0777 lets the container's
// remote user (uid 1000 in the fixtures' image) write into it.
func workspace(t *testing.T, fixture string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if out, err := exec.Command("cp", "-a", fixture, ws).CombinedOutput(); err != nil {
		t.Fatalf("copying %s: %v: %s", fixture, err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.Chmod(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	return ws
}

// signatureFixtures are the configurations whose capture signature is
// recorded: every integration fixture that is a plain devcontainer, and this
// package's own. The ones with published features fetch them from ghcr.io.
var signatureFixtures = []struct {
	name string
	path string // relative to the module root
}{
	{"fixture", "integration/fixture"},
	{"fixture-lifecycle", "integration/fixture-lifecycle"},
	{"fixture-dockerfile", "integration/fixture-dockerfile"},
	{"fixture-hostgw", "integration/fixture-hostgw"},
	{"fixture-go-feature", "integration/fixture-go-feature"},
	{"fixture-dind", "integration/fixture-dind"},
	{"hooks", "internal/backend/kubernetes/devcli/testdata/fixtures/hooks"},
	{"runargs", "internal/backend/kubernetes/devcli/testdata/fixtures/runargs"},
}

// signature is what one fixture's capture-up asked of docker: the calls it
// made (by verb) and the docker run options it emitted (by name).
type signature struct {
	Calls    []string `json:"calls"`
	RunFlags []string `json:"runFlags"`
}

// signatureFile is one CLI version's recorded signatures.
type signatureFile struct {
	CLI      string               `json:"cli"`
	Fixtures map[string]signature `json:"fixtures"`
}

func signaturePath(version string) string {
	return filepath.Join("testdata", "signature", "devcontainer-cli-"+version+".json")
}

func loadSignatures(t *testing.T, version string) (*signatureFile, bool) {
	t.Helper()
	data, err := os.ReadFile(signaturePath(version))
	if errors.Is(err, os.ErrNotExist) {
		return &signatureFile{CLI: version, Fixtures: map[string]signature{}}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var f signatureFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s: %v", signaturePath(version), err)
	}
	return &f, true
}

// signatureOf summarises a capture-up.
func signatureOf(res *CaptureResult) signature {
	seen := map[string]bool{}
	var calls []string
	for _, c := range res.Calls {
		v := c.Verb() + " -> " + string(c.Outcome)
		if !seen[v] {
			seen[v] = true
			calls = append(calls, v)
		}
	}
	sort.Strings(calls)
	return signature{Calls: calls, RunFlags: res.Run.Names()}
}

// TestCaptureSignature is the capture-signature test: for each fixture it
// runs capture-up with the installed CLI and checks the docker calls and run
// options against the golden recorded for that CLI version. A new CLI version
// fails until its signature is reviewed and recorded, which is the moment to
// check that the pod-spec translator knows every option it emits. It also
// checks what a capture holds for each.
func TestCaptureSignature(t *testing.T) {
	requireCLI(t)
	t.Setenv("KUBESHIM_FROM_HOST", "from-host")
	root := repoRoot(t)
	update := os.Getenv("FLEET_KUBESHIM_UPDATE") == "1"
	var version string
	var golden *signatureFile
	var haveGolden bool
	for _, fx := range signatureFixtures {
		t.Run(fx.name, func(t *testing.T) {
			ws := workspace(t, filepath.Join(root, fx.path))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			res, err := CaptureUp(ctx, CaptureRequest{WorkspaceDir: ws})
			if err != nil {
				t.Fatalf("CaptureUp: %v", err)
			}
			checkCapture(t, fx.name, ws, res)
			if golden == nil {
				version = res.CLIVersion
				golden, haveGolden = loadSignatures(t, version)
			}
			got := signatureOf(res)
			if update {
				golden.Fixtures[fx.name] = got
				return
			}
			if !haveGolden {
				t.Fatalf("devcontainer CLI %s has no capture signature (%s). Review what it emits, check the pod-spec translator knows every run option, then record it with FLEET_KUBESHIM_UPDATE=1 and add it to TestedCLIVersions:\n%s", version, signaturePath(version), mustJSON(got))
			}
			want, ok := golden.Fixtures[fx.name]
			if !ok {
				t.Fatalf("%s has no signature for %s; record it with FLEET_KUBESHIM_UPDATE=1:\n%s", signaturePath(version), fx.name, mustJSON(got))
			}
			if mustJSON(got) != mustJSON(want) {
				t.Errorf("capture signature changed for %s on devcontainer CLI %s\nnew: %s\ngone: %s\ngot:\n%s",
					fx.name, version, strings.Join(minus(got.all(), want.all()), ", "), strings.Join(minus(want.all(), got.all()), ", "), mustJSON(got))
			}
		})
	}
	if update && golden != nil {
		if err := os.WriteFile(signaturePath(version), append([]byte(mustJSON(golden)), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s", signaturePath(version))
	}
}

// all is a signature's entries, for a diff.
func (s signature) all() []string {
	out := append([]string(nil), s.Calls...)
	for _, f := range s.RunFlags {
		out = append(out, "--"+f)
	}
	return out
}

// minus is a without b.
func minus(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func mustJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(data)
}

// checkCapture checks what every capture holds, and what the fixtures that
// exist for it hold in particular.
func checkCapture(t *testing.T, name, ws string, res *CaptureResult) {
	t.Helper()
	c := res.Capture
	if c.Verb != "run" || c.Format != kubeshim.CaptureFormat {
		t.Fatalf("capture verb %q format %d", c.Verb, c.Format)
	}
	if res.Image.ID == "" || res.Run.Image == "" {
		t.Fatalf("no image: run %q, inspect %+v", res.Run.Image, res.Image)
	}
	if res.Container.Labels["devcontainer.metadata"] == "" {
		t.Errorf("the container has no devcontainer.metadata label (image's or run's)")
	}
	if got := res.Container.Labels["devcontainer.local_folder"]; got != ws {
		t.Errorf("devcontainer.local_folder = %q, want %q", got, ws)
	}
	if res.Config.WorkspaceFolder == "" {
		t.Errorf("no container workspace folder")
	}
	for _, call := range res.Calls {
		if call.Outcome == kubeshim.OutcomeRefused {
			t.Errorf("the shim refused a call: %+v", call)
		}
	}
	redacted := mustJSON(c.Redacted())
	for _, e := range c.Env {
		if !e.Unset && len(e.Value) > 3 && strings.Contains(redacted, e.Value) {
			t.Errorf("the redacted capture still holds the value of %s", e.Name)
		}
	}
	env := map[string]kubeshim.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	switch name {
	case "fixture-dockerfile":
		if env["ITEST_CONTAINER_ENV"].Value != "from-containerEnv" {
			t.Errorf("containerEnv not captured: %+v", c.Env)
		}
		// The local feature is built into the image: its metadata entry is
		// on the image's label and its containerEnv in the image's ENV.
		if !strings.Contains(res.Container.Labels["devcontainer.metadata"], "./itest-feature") {
			t.Errorf("the built image's metadata (the feature) is not in the container's labels")
		}
		if !contains(res.Container.Env, "ITEST_FEATURE_ENV=from-feature") {
			t.Errorf("the feature's containerEnv is not in the container's environment: %v", res.Container.Env)
		}
	case "runargs":
		if env["KUBESHIM_ENV_FILE"].Value != "from-env-file" || !strings.HasPrefix(env["KUBESHIM_ENV_FILE"].Source, "file:") {
			t.Errorf("--env-file not folded in: %+v", env["KUBESHIM_ENV_FILE"])
		}
		if e := env["KUBESHIM_FROM_HOST"]; e.Value != "from-host" || e.Source != "host" {
			t.Errorf("-e NAME not taken from the host: %+v", e)
		}
		if env["KUBESHIM_SPACES"].Value != "x y" {
			t.Errorf("a value with a space: %+v", env["KUBESHIM_SPACES"])
		}
		if res.Container.Labels["kubeshim.fixture"] != "runargs" {
			t.Errorf("runArgs label missing: %v", res.Container.Labels)
		}
		if u, _ := res.Run.Value("user"); u != "vscode" {
			t.Errorf("-u = %q, want the containerUser", u)
		}
		if res.Config.WorkspaceFolder != "/work/kubeshim" {
			t.Errorf("workspaceFolder = %q", res.Config.WorkspaceFolder)
		}
		if res.RemoteUser != "vscode" {
			t.Errorf("remote user = %q", res.RemoteUser)
		}
	case "fixture-go-feature":
		if !res.Run.Bool("init", false) || len(res.Run.Values("cap-add")) == 0 {
			t.Errorf("the go feature's init and SYS_PTRACE are not on the run: %v", res.Run.Flags)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestReadMergedRefusesCompose: a compose devcontainer fails before any
// build, through ReadMerged and through a CaptureUp that reads the
// configuration itself.
func TestReadMergedRefusesCompose(t *testing.T) {
	requireCLI(t)
	ws := workspace(t, filepath.Join("testdata", "fixtures", "compose"))
	ctx := context.Background()
	_, err := ReadMerged(ctx, ReadRequest{WorkspaceDir: ws})
	if !IsCompose(err) || !strings.HasPrefix(err.Error(), kubeshim.UnsupportedCompose) {
		t.Fatalf("ReadMerged: %v, want the compose refusal", err)
	}
	if _, err := CaptureUp(ctx, CaptureRequest{WorkspaceDir: ws}); !IsCompose(err) {
		t.Fatalf("CaptureUp: %v, want the compose refusal", err)
	}
}

// TestSetUpReplay is the pod-mode shim against the real CLI and docker: the
// captured run is replayed on docker as a stand-in pod, and set-up runs the
// hooks with inspect answered by the shim from the capture and every exec sent
// through the exec socket to `docker exec` in that container. It checks the
// marker semantics a pod depends on: the creation hooks once, postStart once
// per run, postAttach every time; and that the hooks see the remote user,
// the workspace folder, containerEnv and remoteEnv.
func TestSetUpReplay(t *testing.T) {
	requireCLI(t)
	ws := workspace(t, filepath.Join("testdata", "fixtures", "hooks"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := CaptureUp(ctx, CaptureRequest{WorkspaceDir: ws})
	if err != nil {
		t.Fatalf("CaptureUp: %v", err)
	}
	cid := replay(t, res)
	ex := dockerExecer{container: cid}

	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	started := time.Now().UTC()
	container := res.Container
	container.ContainerID = "k8s:test/fleet-it/fi-kubeshim-replay@g1"
	container.Name = "fi-kubeshim-replay-0"
	container.Created = created
	container.StartedAt = kubeshim.StartedAt(started, "pod-uid-1", 0)

	setUp := func(c kubeshim.Container) *SetUpResult {
		t.Helper()
		out, err := SetUp(ctx, SetUpRequest{Container: c, WorkspaceFolder: res.Config.WorkspaceFolder, Execer: ex})
		if err != nil {
			t.Fatalf("SetUp: %v", err)
		}
		for _, call := range out.Calls {
			t.Logf("set-up called docker %s -> %s", call.Verb(), call.Outcome)
			switch call.Outcome {
			case kubeshim.OutcomeInspect, kubeshim.OutcomeExec, kubeshim.OutcomeNoEngine:
			default:
				t.Errorf("pod-mode call %v -> %s; set-up must only inspect and exec", call.Args, call.Outcome)
			}
		}
		return out
	}

	first := setUp(container)
	if len(first.Merged) == 0 {
		t.Errorf("set-up returned no merged configuration")
	}
	assertHooks(t, ex, res.Config.WorkspaceFolder, "onCreate updateContent postCreate postStart postAttach")

	setUp(container) // the same run again: nothing but postAttach
	assertHooks(t, ex, res.Config.WorkspaceFolder, "onCreate updateContent postCreate postStart postAttach postAttach")

	restarted := container // a container restart inside the same second
	restarted.StartedAt = kubeshim.StartedAt(started, "pod-uid-1", 1)
	setUp(restarted)
	assertHooks(t, ex, res.Config.WorkspaceFolder, "onCreate updateContent postCreate postStart postAttach postAttach postStart postAttach")

	envOut := ex.run(t, "root", "cat", res.Config.WorkspaceFolder+"/.hooks.env")
	want := strings.Join([]string{"vscode", res.Config.WorkspaceFolder, "from-containerEnv with spaces", "from-remoteEnv", "from-containerEnv with spaces"}, "\n")
	if strings.TrimSpace(envOut) != want {
		t.Errorf("the hooks saw:\n%s\nwant:\n%s", envOut, want)
	}
	owner := ex.run(t, "root", "stat", "-c", "%U", DefaultDataFolder)
	if strings.TrimSpace(owner) != "vscode" {
		t.Errorf("%s is owned by %q, want the remote user", DefaultDataFolder, owner)
	}
	markers := ex.run(t, "root", "cat", DefaultDataFolder+"/.onCreateCommandMarker", DefaultDataFolder+"/.postStartCommandMarker")
	if !strings.Contains(markers, "2026-10-01T12:00:00Z") || !strings.Contains(markers, restarted.StartedAt.Format(time.RFC3339Nano)) {
		t.Errorf("markers hold %q, want Created and the last StartedAt", markers)
	}
}

// assertHooks checks the hooks log in the stand-in pod.
func assertHooks(t *testing.T, ex dockerExecer, wsFolder, want string) {
	t.Helper()
	got := strings.Fields(ex.run(t, "root", "cat", wsFolder+"/.hooks.log"))
	if strings.Join(got, " ") != want {
		t.Errorf("hooks ran %q, want %q", strings.Join(got, " "), want)
	}
}

// replay runs a captured docker run on docker, detached, as a stand-in for the
// pod, and removes it when the test ends.
func replay(t *testing.T, res *CaptureResult) string {
	t.Helper()
	args := []string{"run", "-d", "--label", "fleet.kubeshim.test=replay"}
	for _, f := range res.Run.Flags {
		switch f.Name {
		case "attach", "sig-proxy":
			continue
		}
		args = append(args, "--"+f.Name+"="+f.Value)
	}
	args = append(args, res.Run.Image)
	args = append(args, res.Run.Cmd...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("replaying the captured run: %v: %s", err, out)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	return cid
}

// dockerExecer is an Execer over `docker exec` into a stand-in container.
type dockerExecer struct {
	container string
}

func (d dockerExecer) Exec(ctx context.Context, req kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	args := []string{"exec"}
	if req.Stdin {
		args = append(args, "-i")
	}
	if req.TTY {
		args = append(args, "-t")
	}
	if req.User != "" {
		args = append(args, "-u", req.User)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	if req.Dir != "" {
		args = append(args, "-w", req.Dir)
	}
	args = append(append(args, d.container), req.Argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = stdio.Stdout, stdio.Stderr
	if stdio.Stdin != nil {
		w, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		go func() {
			_, _ = io.Copy(w, stdio.Stdin)
			_ = w.Close()
		}()
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// run runs a command in the stand-in container and returns its output.
func (d dockerExecer) run(t *testing.T, user string, argv ...string) string {
	t.Helper()
	var out bytes.Buffer
	code, err := d.Exec(context.Background(), kubeshim.ExecRequest{User: user, Argv: argv}, kubeshim.ExecStdio{Stdout: &out, Stderr: &out})
	if err != nil || code != 0 {
		t.Fatalf("%v: exit %d, %v: %s", argv, code, err, out.String())
	}
	return out.String()
}
