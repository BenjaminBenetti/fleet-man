package kubeshim

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testShim is a shim directory driven in-process.
type testShim struct {
	t   *testing.T
	dir *Dir
	// ppid is what the shim takes for its parent's pid.
	ppid atomic.Int64
	// exec records what a passthrough handed to the real docker.
	execPath string
	execArgv []string
	env      map[string]string
	cwd      string
}

func newTestShim(t *testing.T, cfg Config) *testShim {
	t.Helper()
	d, err := NewDir("/usr/bin/fleet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove() })
	if cfg.Mode == ModePod && cfg.Pod.ExecSocket == "" {
		cfg.Pod.ExecSocket = d.ExecSocket()
	}
	if err := d.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	s := &testShim{t: t, dir: d, env: map[string]string{}, cwd: t.TempDir()}
	s.ppid.Store(4242)
	return s
}

// call runs `<binary> args...` through the shim and returns its exit code,
// stdout and stderr.
func (s *testShim) call(binary string, stdin string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(invocation{
		args:   append([]string{filepath.Join(s.dir.Path(), binary)}, args...),
		stdin:  strings.NewReader(stdin),
		stdout: &stdout,
		stderr: &stderr,
		getenv: func(k string) (string, bool) {
			v, ok := s.env[k]
			return v, ok
		},
		environ: func() []string { return []string{"A=1"} },
		cwd:     s.cwd,
		exec: func(path string, argv []string, env []string) error {
			s.execPath, s.execArgv = path, argv
			return os.ErrPermission // a real exec never returns
		},
		isTerminal: func(io.Reader) bool { return false },
		getppid:    func() int { return int(s.ppid.Load()) },
		poll:       10 * time.Millisecond,
	})
	return code, stdout.String(), stderr.String()
}

func (s *testShim) docker(args ...string) (int, string, string) {
	return s.call(dockerLink, "", args...)
}

func (s *testShim) outcomes() []Outcome {
	s.t.Helper()
	calls, err := s.dir.Calls()
	if err != nil {
		s.t.Fatal(err)
	}
	var out []Outcome
	for _, c := range calls {
		out = append(out, c.Outcome)
	}
	return out
}

func TestCaptureModePassesBuildsToTheRealDocker(t *testing.T) {
	for _, args := range [][]string{
		{"buildx", "version"},
		{"buildx", "build", "--load", "-t", "img", "."},
		{"build", "-t", "img", "."},
		{"image", "inspect", "img"},
		{"inspect", "--type", "image", "img"},
		{"inspect", "--type=image", "img"},
		{"pull", "img"},
		{"tag", "a", "b"},
		{"login", "registry"},
		{"version", "--format", "{{.Server.Version}}"},
		{"-v"},
		{"--context", "ctx", "buildx", "build", "."},
	} {
		s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
		code, _, _ := s.docker(args...)
		if s.execPath != "/usr/bin/docker" || !reflect.DeepEqual(s.execArgv, append([]string{"/usr/bin/docker"}, args...)) {
			t.Errorf("%v: exec %q %q, want the real docker with the same argv", args, s.execPath, s.execArgv)
		}
		if code != exitShim {
			t.Errorf("%v: a failed exec should be the shim's failure, got %d", args, code)
		}
		if got := s.outcomes(); !reflect.DeepEqual(got, []Outcome{OutcomePass}) {
			t.Errorf("%v: journal %v", args, got)
		}
	}
}

func TestCaptureModeEngineHasNoContainers(t *testing.T) {
	s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
	if code, out, _ := s.docker("ps", "-q", "-a", "--filter", "label=devcontainer.local_folder=/ws"); code != 0 || out != "" {
		t.Errorf("ps: %d %q, want an empty list", code, out)
	}
	if code, out, _ := s.docker("container", "ls", "-q"); code != 0 || out != "" {
		t.Errorf("container ls: %d %q", code, out)
	}
	code, out, errOut := s.docker("inspect", "--type", "container", "abc")
	if code != 1 || strings.TrimSpace(out) != "[]" || !strings.Contains(errOut, "No such container: abc") {
		t.Errorf("inspect container: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := s.docker("container", "inspect", "abc"); code != 1 || !strings.Contains(errOut, "No such container: abc") {
		t.Errorf("container inspect: %d %q", code, errOut)
	}
	if s.execPath != "" {
		t.Errorf("container queries reached the real docker: %q", s.execArgv)
	}
	want := []Outcome{OutcomeEmpty, OutcomeEmpty, OutcomeMissing, OutcomeMissing}
	if got := s.outcomes(); !reflect.DeepEqual(got, want) {
		t.Errorf("journal %v, want %v", got, want)
	}
}

func TestCaptureModeCapturesTheRun(t *testing.T) {
	s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
	s.env["FROM_HOST"] = "host value"
	envFile := filepath.Join(s.cwd, "app.env")
	if err := os.WriteFile(envFile, []byte("\xef\xbb\xbf# comment\n\n  FILE_A=from file\nOVERRIDDEN=file\nBARE_SET\nBARE_UNSET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.env["BARE_SET"] = "bare"
	labelFile := filepath.Join(s.cwd, "labels")
	if err := os.WriteFile(labelFile, []byte("from.file=1\nflagwins=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--sig-proxy=false", "-a", "STDOUT",
		"--env-file", "app.env", "--label-file", labelFile,
		"-e", "SECRET=s3cr3t", "-e", "OVERRIDDEN=flag", "-e", "FROM_HOST", "-e", "NOT_ON_HOST",
		"-l", "flagwins=flag", "-l", "bare",
		"-l", `devcontainer.metadata=[{"remoteEnv":{"TOKEN":"t0k3n"}}]`,
		"--entrypoint", "/bin/sh", "img:1", "-c", "echo Container started", "-"}
	code, _, errOut := s.docker(append([]string{"run"}, args...)...)
	if code != ExitCaptured || !strings.Contains(errOut, "captured docker run") {
		t.Fatalf("run: exit %d %q, want %d", code, errOut, ExitCaptured)
	}
	c, err := s.dir.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb != "run" || !reflect.DeepEqual(c.Args, args) || c.Format != CaptureFormat {
		t.Errorf("capture %q %q", c.Verb, c.Args)
	}
	wantEnv := []EnvVar{
		{Name: "FILE_A", Value: "from file", Source: "file:app.env"},
		{Name: "OVERRIDDEN", Value: "flag", Source: "flag"},
		{Name: "BARE_SET", Value: "bare", Source: "file:app.env"},
		{Name: "BARE_UNSET", Unset: true, Source: "file:app.env"},
		{Name: "SECRET", Value: "s3cr3t", Source: "flag"},
		{Name: "FROM_HOST", Value: "host value", Source: "host"},
		{Name: "NOT_ON_HOST", Unset: true, Source: "host"},
	}
	if !reflect.DeepEqual(c.Env, wantEnv) {
		t.Errorf("env\n got %+v\nwant %+v", c.Env, wantEnv)
	}
	wantLabels := map[string]string{"from.file": "1", "flagwins": "flag", "bare": "", "devcontainer.metadata": `[{"remoteEnv":{"TOKEN":"t0k3n"}}]`}
	if !reflect.DeepEqual(c.Labels, wantLabels) {
		t.Errorf("labels %v", c.Labels)
	}
	if got := c.EnvList(); got[len(got)-1] != "NOT_ON_HOST" || got[0] != "FILE_A=from file" {
		t.Errorf("EnvList %q", got)
	}
	info, err := os.Stat(filepath.Join(s.dir.Path(), captureFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("capture file mode: %v %v", info, err)
	}

	// A second run is refused rather than overwriting the first.
	if code, _, _ := s.docker("run", "img"); code != exitShim {
		t.Errorf("second run: exit %d", code)
	}
	if got := s.outcomes(); !reflect.DeepEqual(got, []Outcome{OutcomeCaptured, OutcomeRefused}) {
		t.Errorf("journal %v", got)
	}
}

func TestRedactedCaptureHidesEveryValue(t *testing.T) {
	c := &Capture{
		Format: CaptureFormat, Verb: "run",
		Args: []string{"-e", "SECRET=s3cr3t", "--env=OTHER=0th3r", "-l", `devcontainer.metadata=[{"containerEnv":{"K":"m3t4"}}]`, "img"},
		Env:  []EnvVar{{Name: "SECRET", Value: "s3cr3t", Source: "flag"}, {Name: "GONE", Unset: true, Source: "host"}},
		Labels: map[string]string{
			"devcontainer.metadata": `[{"containerEnv":{"K":"m3t4"}}]`,
			"plain":                 "kept",
		},
	}
	r := c.Redacted()
	data, _ := json.Marshal(r)
	for _, secret := range []string{"s3cr3t", "0th3r", "m3t4"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("redacted capture holds %q: %s", secret, data)
		}
	}
	for _, kept := range []string{"SECRET=", "OTHER=", "kept", "GONE"} {
		if !strings.Contains(string(data), kept) {
			t.Errorf("redacted capture lost %q: %s", kept, data)
		}
	}
	if want := `[{"containerEnv":{"K":"<redacted>"}}]`; r.Labels["devcontainer.metadata"] != want {
		t.Errorf("metadata label = %s, want %s", r.Labels["devcontainer.metadata"], want)
	}
	if c.Env[0].Value != "s3cr3t" || c.Args[1] != "SECRET=s3cr3t" {
		t.Errorf("Redacted changed the original")
	}
}

func TestCaptureModeRefusesAnUnknownRunFlag(t *testing.T) {
	s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
	code, _, errOut := s.docker("run", "--frobnicate", "img")
	if code != exitShim || !strings.Contains(errOut, "kubernetes:unsupported:flag:frobnicate") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if _, err := s.dir.Capture(); err != ErrNoCapture {
		t.Errorf("an unparseable run was captured: %v", err)
	}
	calls, _ := s.dir.Calls()
	if len(calls) != 1 || calls[0].Outcome != OutcomeRefused || !strings.Contains(calls[0].Detail, "kubernetes:unsupported:flag:frobnicate") {
		t.Errorf("journal %+v", calls)
	}
}

func TestComposeIsRefusedEverywhere(t *testing.T) {
	for _, mode := range []Config{
		{Mode: ModeCapture, Docker: "/usr/bin/docker"},
		{Mode: ModePod, Pod: &PodConfig{Container: Container{ContainerID: "k8s:c/ns/sts@g1"}}},
	} {
		s := newTestShim(t, mode)
		for _, call := range []struct {
			binary string
			args   []string
		}{
			{dockerLink, []string{"compose", "version", "--short"}},
			{dockerLink, []string{"compose", "-f", "docker-compose.yml", "up", "-d"}},
			{composeLink, []string{"version"}},
			{composeLink, []string{"-f", "x.yml", "config"}},
		} {
			code, _, errOut := s.call(call.binary, "", call.args...)
			if code == 0 || !strings.HasPrefix(errOut, UnsupportedCompose+":") {
				t.Errorf("%s mode %s %v: exit %d %q", mode.Mode, call.binary, call.args, code, errOut)
			}
		}
		if s.execPath != "" {
			t.Errorf("%s mode: compose reached the real docker", mode.Mode)
		}
		for _, o := range s.outcomes() {
			if o != OutcomeCompose {
				t.Errorf("%s mode journal %v", mode.Mode, s.outcomes())
				break
			}
		}
	}
}

func TestCaptureModeRefusesTheRest(t *testing.T) {
	for _, args := range [][]string{
		{}, {"exec", "c", "true"}, {"start", "c"}, {"rm", "-f", "c"}, {"stop", "c"}, {"info"},
		{"push", "img"}, {"inspect", "c"}, {"container", "start", "c"}, {"network", "create", "n"},
		{"--bogus", "ps"},
	} {
		s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
		code, _, errOut := s.docker(args...)
		if code != exitShim || !strings.Contains(errOut, "kubeshim (capture mode): refusing") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
		if s.execPath != "" {
			t.Errorf("%v reached the real docker", args)
		}
	}
}

func TestCaptureModeEventsBlocks(t *testing.T) {
	// events runs until its directory goes, or its parent does.
	for _, tc := range []struct {
		name string
		end  func(s *testShim)
	}{
		{"the directory goes", func(s *testShim) { _ = os.Remove(filepath.Join(s.dir.Path(), ConfigFile)) }},
		{"the parent goes", func(s *testShim) { s.ppid.Store(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
			done := make(chan int, 1)
			go func() {
				code, _, _ := s.docker("events", "--format", "{{json .}}", "--filter", "event=start")
				done <- code
			}()
			select {
			case code := <-done:
				t.Fatalf("events returned %d while its directory and parent exist", code)
			case <-time.After(100 * time.Millisecond):
			}
			tc.end(s)
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("events exited %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("events kept blocking")
			}
		})
	}

	// Already reparented to init when it looked: its parent is gone.
	s := newTestShim(t, Config{Mode: ModeCapture, Docker: "/usr/bin/docker"})
	s.ppid.Store(1)
	if code, _, _ := s.docker("events"); code != 0 {
		t.Errorf("an orphaned events exited %d", code)
	}
}

func TestShimOutsideAShimDirectory(t *testing.T) {
	var stderr bytes.Buffer
	code := run(invocation{args: []string{filepath.Join(t.TempDir(), "docker"), "ps"}, stderr: &stderr})
	if code != exitShim || !strings.Contains(stderr.String(), ConfigFile) {
		t.Errorf("exit %d %q", code, stderr.String())
	}
}

func TestInvoked(t *testing.T) {
	for argv0, want := range map[string]bool{
		"/tmp/fleet-kubeshim-1/docker":         true,
		"/tmp/fleet-kubeshim-1/docker-compose": true,
		"docker":                               true,
		"/usr/bin/fleet":                       false,
		"fleet":                                false,
		"/tmp/dockerd":                         false,
	} {
		if got := Invoked(argv0); got != want {
			t.Errorf("Invoked(%q) = %v", argv0, got)
		}
	}
}

func TestCallVerb(t *testing.T) {
	for _, tc := range []struct {
		call Call
		want string
	}{
		{Call{Binary: "docker", Args: []string{"-v"}}, "-v"},
		{Call{Binary: "docker", Args: []string{"buildx", "build", "--load", "."}}, "buildx build"},
		{Call{Binary: "docker", Args: []string{"--context", "x", "buildx", "version"}}, "buildx version"},
		{Call{Binary: "docker", Args: []string{"inspect", "--type", "image", "img"}}, "inspect --type image"},
		{Call{Binary: "docker", Args: []string{"inspect", "img"}}, "inspect"},
		{Call{Binary: "docker", Args: []string{"run", "--rm", "img"}}, "run"},
		{Call{Binary: "docker", Args: []string{"container", "inspect", "c"}}, "container inspect"},
		{Call{Binary: "docker-compose", Args: []string{"up"}}, "docker-compose"},
	} {
		if got := tc.call.Verb(); got != tc.want {
			t.Errorf("Verb(%v) = %q, want %q", tc.call.Args, got, tc.want)
		}
	}
}
