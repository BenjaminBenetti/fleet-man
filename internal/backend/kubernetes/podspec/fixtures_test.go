package podspec

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

// The fixture goldens: each integration fixture's real capture-up (recorded
// from the devcontainer CLI into testdata/captures) translated for a local
// cluster and for a managed one, into testdata/golden.

// recordedCapture is one fixture's capture-up as testdata/captures holds it:
// what Translate reads, with the host paths of the recording replaced by
// fixed ones.
type recordedCapture struct {
	CLI             string               `json:"cli"`
	WorkspaceDir    string               `json:"workspaceDir"`
	WorkspaceFolder string               `json:"workspaceFolder"`
	Capture         *kubeshim.Capture    `json:"capture"`
	Image           kubeshim.ImageConfig `json:"image"`
	Merged          json.RawMessage      `json:"merged"`
}

// Where a recorded capture's host paths point: the workspace, and the
// daemon-host files the host-socket and host-bind fixtures bind.
const (
	recordedSrc  = "/home/dev/src"
	recordedHost = "/home/dev/host"
)

// captureFixtures are the recorded fixtures, with the host environment their
// devcontainer.json reads (paths relative to the recording's host dir).
var captureFixtures = []struct {
	name string
	path string // relative to the module root
	env  map[string]string
}{
	{name: "fixture", path: "integration/fixture"},
	{name: "fixture-lifecycle", path: "integration/fixture-lifecycle"},
	{name: "fixture-dockerfile", path: "integration/fixture-dockerfile"},
	{name: "fixture-hostgw", path: "integration/fixture-hostgw"},
	{name: "fixture-go-feature", path: "integration/fixture-go-feature"},
	{name: "go-image", path: "internal/backend/kubernetes/podspec/testdata/fixtures/go-image"},
	{name: "fixture-dind", path: "integration/fixture-dind"},
	{name: "fixture-dood", path: "integration/fixture-dood"},
	{name: "fixture-twouser", path: "integration/fixture-twouser"},
	{name: "fixture-hostsock", path: "integration/fixture-hostsock",
		env: map[string]string{"SSH_AUTH_SOCK": "agent.sock", "FLEET_ITEST_GPG_SOCK": "gnupg/S.gpg-agent",
			"FLEET_ITEST_SOCK_DIR": "sockets", "FLEET_ITEST_LOG_SOCK": "log.sock"}},
	{name: "fixture-hostbind", path: "integration/fixture-hostbind",
		env: map[string]string{"FLEET_ITEST_HOSTBIND_DIR": "hostbind"}},
	{name: "hooks", path: "internal/backend/kubernetes/devcli/testdata/fixtures/hooks"},
	{name: "runargs", path: "internal/backend/kubernetes/devcli/testdata/fixtures/runargs"},
}

// recordedHostFS is the daemon host the recorded captures bind from.
var recordedHostFS = fakeFS{
	recordedHost:                               fs.ModeDir | 0o755,
	recordedHost + "/agent.sock":               fs.ModeSocket | 0o600,
	recordedHost + "/gnupg":                    fs.ModeDir | 0o700,
	recordedHost + "/gnupg/S.gpg-agent":        fs.ModeSocket | 0o600,
	recordedHost + "/gnupg/pubring.kbx":        0o600,
	recordedHost + "/sockets":                  fs.ModeDir | 0o755,
	recordedHost + "/sockets/X0":               fs.ModeSocket | 0o666,
	recordedHost + "/log.sock":                 fs.ModeSocket | 0o666,
	recordedHost + "/hostbind":                 fs.ModeDir | 0o755,
	recordedHost + "/hostbind/aws":             fs.ModeDir | 0o700,
	recordedHost + "/hostbind/aws/credentials": 0o600,
	recordedHost + "/hostbind/gitconfig":       0o644,
}

// profiles are the connections each fixture is translated for.
var profiles = []struct {
	name  string
	in    func(*Input)
	extra bool // only for the fixtures that ask for it
}{
	{name: "minikube", in: func(in *Input) {
		in.Cluster = &fleet.KubeCluster{ID: "minikube", Namespace: "fleet"}
		in.Facts = ClusterFacts{Local: true, RuntimeClasses: []string{},
			Nodes: []Node{{Arch: "amd64", AllocatableCPU: "4", AllocatableMemory: "8Gi"}}}
	}},
	{name: "managed", in: func(in *Input) {
		in.Cluster = &fleet.KubeCluster{ID: "c-prod", DisplayName: "prod-eu", Namespace: "team-dev"}
		in.Facts = ClusterFacts{LimitRangeMemoryLimit: "1Gi", RuntimeClasses: []string{"gvisor"},
			Nodes: []Node{{Arch: "amd64", AllocatableCPU: "7910m", AllocatableMemory: "29Gi"}, {Arch: "arm64", AllocatableCPU: "16", AllocatableMemory: "64Gi"}}}
	}},
}

func loadRecorded(t *testing.T, name string) *recordedCapture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "captures", name+".json"))
	if err != nil {
		t.Fatalf("%v (record it with FLEET_KUBESHIM_CLI=1 FLEET_PODSPEC_UPDATE=1 go test -run TestRecordCaptures)", err)
	}
	var rc recordedCapture
	if err := json.Unmarshal(data, &rc); err != nil {
		t.Fatal(err)
	}
	return &rc
}

// input is the Input of a recorded capture.
func (rc *recordedCapture) input() Input {
	return Input{
		Capture:          rc.Capture,
		Image:            rc.Image,
		Merged:           rc.Merged,
		WorkspaceDir:     rc.WorkspaceDir,
		WorkspaceFolder:  rc.WorkspaceFolder,
		Host:             recordedHostFS,
		HostAgentSockets: []string{recordedHost + "/agent.sock"},
		SSHAuthSock:      "/fleet-mounts/control/ssh-agent.sock",
	}
}

// goldenResult is a translation as a golden file shows it: the pod and its
// Secrets (spec files decoded; pid1.json and merged.json left out, being
// the pod's PID1 and the input verbatim), or the error.
type goldenResult struct {
	Error     string            `json:"error,omitempty"`
	Refusals  []string          `json:"refusals,omitempty"`
	Pod       *PodSpec          `json:"pod,omitempty"`
	SpecFiles map[string]any    `json:"specFiles,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func goldenOf(t *testing.T, in Input, res *Result, err error) goldenResult {
	t.Helper()
	if err != nil {
		g := goldenResult{Error: err.Error()}
		for _, r := range Refusals(err) {
			g.Refusals = append(g.Refusals, r.Code)
		}
		return g
	}
	g := goldenResult{Pod: &res.Pod, SpecFiles: map[string]any{}, Env: map[string]string{}}
	for name, data := range res.Secrets.Spec {
		switch name {
		case MergedFile:
			if string(data) != strings.TrimSpace(string(in.Merged)) {
				t.Errorf("merged.json is not the merged configuration verbatim")
			}
			continue
		case PID1File:
			var pid1 PID1
			if err := json.Unmarshal(data, &pid1); err != nil || !reflect.DeepEqual(pid1, res.Pod.PID1) {
				t.Errorf("pid1.json is not the pod's PID1: %s", data)
			}
			checkPID1Boots(t, "pid1.json", res)
			continue
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		g.SpecFiles[name] = v
	}
	for k, v := range res.Secrets.Env {
		g.Env[k] = string(v)
	}
	return g
}

func TestFixtureGoldens(t *testing.T) {
	for _, fx := range captureFixtures {
		rc := loadRecorded(t, fx.name)
		for _, p := range profiles {
			t.Run(fx.name+"/"+p.name, func(t *testing.T) {
				in := rc.input()
				p.in(&in)
				res, err := Translate(in)
				checkGolden(t, filepath.Join("golden", fx.name+"."+p.name+".json"), mustIndent(t, goldenOf(t, in, res, err)))
			})
		}
	}
}

// The go feature and the mcr Go image ask for SYS_PTRACE and unconfined
// seccomp, which baseline pod security denies: the re-render drops them,
// with the warning.
func TestFixtureGoldenDebugRerender(t *testing.T) {
	for _, name := range []string{"fixture-go-feature", "go-image"} {
		t.Run(name, func(t *testing.T) {
			rc := loadRecorded(t, name)
			in := rc.input()
			profiles[1].in(&in)
			full, err := Translate(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(full.Pod.DebugGrants) == 0 {
				t.Fatal("the fixture asks for no debug grants")
			}
			rerender, err := Denied(in.Cluster, &full.Pod, `pods "x" is forbidden: violates PodSecurity "baseline:latest": non-default capabilities (container "dev" must not include "SYS_PTRACE" in securityContext.capabilities.add), seccompProfile (pod or container "dev" must not set securityContext.seccompProfile.type to "Unconfined")`)
			if err != nil || !rerender {
				t.Fatalf("Denied = %v, %v; want a re-render", rerender, err)
			}
			in.DropDebugGrants = true
			res, err := Translate(in)
			checkGolden(t, filepath.Join("golden", name+".managed-rerender.json"), mustIndent(t, goldenOf(t, in, res, err)))
		})
	}
}

// TestRecordCaptures runs capture-up with the real devcontainer CLI and
// docker for each fixture (FLEET_KUBESHIM_CLI=1, as the devcli real-CLI
// tests). With FLEET_PODSPEC_UPDATE=1 it rewrites testdata/captures; without,
// it checks the recorded captures still stand for what the installed CLI
// emits: the same docker run options, translated the same way.
func TestRecordCaptures(t *testing.T) {
	if os.Getenv("FLEET_KUBESHIM_CLI") != "1" {
		t.Skip("set FLEET_KUBESHIM_CLI=1 to run capture-up with the real devcontainer CLI and docker")
	}
	for _, tool := range []string{"devcontainer", "docker", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("FLEET_KUBESHIM_CLI=1 needs %s on PATH", tool)
		}
	}
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBESHIM_FROM_HOST", "from-host")
	for _, fx := range captureFixtures {
		t.Run(fx.name, func(t *testing.T) {
			host := t.TempDir()
			for k, v := range fx.env {
				t.Setenv(k, filepath.Join(host, v))
			}
			ws := filepath.Join(t.TempDir(), fx.name)
			if out, err := exec.Command("cp", "-a", filepath.Join(root, fx.path), ws).CombinedOutput(); err != nil {
				t.Fatalf("copying the fixture: %v: %s", err, out)
			}
			if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			res, err := devcli.CaptureUp(ctx, devcli.CaptureRequest{WorkspaceDir: ws})
			if err != nil {
				t.Fatalf("CaptureUp: %v", err)
			}
			if image := res.Run.Image; strings.HasPrefix(image, "vsc-") {
				t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })
			}
			rc := normalizeCapture(t, &recordedCapture{
				CLI:             res.CLIVersion,
				WorkspaceDir:    ws,
				WorkspaceFolder: res.Config.WorkspaceFolder,
				Capture:         res.Capture,
				Image:           res.Image,
				Merged:          res.Config.Merged,
			}, ws, filepath.Join(recordedSrc, fx.name), host)
			path := filepath.Join("testdata", "captures", fx.name+".json")
			if updating() {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, mustIndent(t, rc), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want := loadRecorded(t, fx.name)
			got, _ := rc.Capture.Run()
			wantRun, _ := want.Capture.Run()
			if strings.Join(got.Names(), " ") != strings.Join(wantRun.Names(), " ") {
				t.Errorf("devcontainer CLI %s emits options %v; the recorded capture (CLI %s) has %v; re-record with FLEET_PODSPEC_UPDATE=1", rc.CLI, got.Names(), want.CLI, wantRun.Names())
			}
			for _, p := range profiles {
				a, b := rc.input(), want.input()
				p.in(&a)
				p.in(&b)
				ra, errA := Translate(a)
				rb, errB := Translate(b)
				if (errA == nil) != (errB == nil) {
					t.Errorf("%s: the live capture translates with error %v, the recorded one with %v", p.name, errA, errB)
					continue
				}
				if errA == nil && jsonString(ra.Pod.Options) != jsonString(rb.Pod.Options) {
					t.Errorf("%s: option outcomes differ:\nlive     %s\nrecorded %s", p.name, jsonString(ra.Pod.Options), jsonString(rb.Pod.Options))
				}
			}
		})
	}
}

// vscImageRE is the tag the CLI gives an image it builds for a workspace,
// whose hash part depends on the workspace's path.
var vscImageRE = regexp.MustCompile(`(vsc-[a-z0-9._-]+?-)[0-9a-f]{64}`)

// normalizeCapture replaces the recording's paths (the workspace ws, the
// host dir) with fixed ones, so a re-recording only changes what the CLI
// changed. The ${devcontainerId} derived from the workspace path is
// recomputed for the new path.
func normalizeCapture(t *testing.T, rc *recordedCapture, ws, newWS, host string) *recordedCapture {
	t.Helper()
	oldID := devcontainerID(rc.Capture.Labels)
	text := string(mustIndent(t, rc))
	text = strings.ReplaceAll(text, ws, newWS)
	text = strings.ReplaceAll(text, host, recordedHost)
	text = vscImageRE.ReplaceAllString(text, "${1}"+strings.Repeat("0", 64))
	var out recordedCapture
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if newID := devcontainerID(out.Capture.Labels); oldID != "" && newID != oldID {
		text = strings.ReplaceAll(text, oldID, newID)
		out = recordedCapture{}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
	}
	// A temporary path the test made that is still there would change on
	// every recording.
	if i := strings.Index(text, t.Name()); i >= 0 {
		t.Fatalf("the normalized capture still holds a path of the recording: ...%s...", text[max(0, i-40):min(len(text), i+80)])
	}
	return &out
}

// checkPID1Boots fails when pod-init would refuse the pid1.json a translation
// rendered: the pod would crash-loop before the start script runs.
func checkPID1Boots(t *testing.T, name string, res *Result) {
	t.Helper()
	file := filepath.Join(t.TempDir(), PID1File)
	if err := os.WriteFile(file, res.Secrets.Spec[PID1File], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := podinit.LoadSpec(file); err != nil {
		t.Errorf("%s: pod-init refuses the rendered pid1.json: %v", name, err)
	}
}
