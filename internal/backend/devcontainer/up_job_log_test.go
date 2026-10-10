package devcontainer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// fakeDevcontainerCLI puts a `devcontainer` and a `docker` on PATH. The CLI
// logs a docker run line carrying a secret -e value to stderr, records its
// argv, prints a successful result to stdout, and exits with exitCode.
func fakeDevcontainerCLI(t *testing.T, exitCode int) (argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixtures require Unix")
	}
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	t.Setenv("UP_TEST_ARGV", argvLog)
	t.Setenv(sshAgentSockOverrideEnv, "off")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cli := `#!/bin/sh
printf '%s\n' "$@" > "$UP_TEST_ARGV"
echo '[1 ms] @devcontainers/cli 0.0.0. Node.js v0.' >&2
echo '[2 ms] Start: Run: docker run --sig-proxy=false -l devcontainer.local_folder=/ws -e API_KEY=hunter2-secret --entrypoint /bin/sh -l devcontainer.metadata=[{"containerEnv":{"API_KEY":"hunter2-secret"}}] img -c echo Container started' >&2
printf 'Container started\n' >&2
`
	if exitCode == 0 {
		cli += `printf '{"outcome":"success","containerId":"cid-123","remoteUser":"node","remoteWorkspaceFolder":"/workspaces/ws"}\n'` + "\n"
	} else {
		cli += "echo 'Error: docker run failed' >&2\nexit " + strconv.Itoa(exitCode) + "\n"
	}
	for name, script := range map[string]string{"devcontainer": cli, "docker": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return argvLog
}

func TestUpContextTeesTheCLILogIntoTheJobLog(t *testing.T) {
	fakeDevcontainerCLI(t, 0)
	var jobLog bytes.Buffer
	ctx := backend.WithJobLog(context.Background(), &jobLog)

	res, err := New().UpContext(ctx, backend.UpRequest{WorkspaceDir: t.TempDir()})
	if err != nil {
		t.Fatalf("UpContext: %v", err)
	}
	if res.ContainerID != "cid-123" {
		t.Fatalf("container = %q, want the CLI's result", res.ContainerID)
	}
	got := jobLog.String()
	for _, want := range []string{"@devcontainers/cli", "Start: Run: docker run", "Container started\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("job log is missing %q:\n%s", want, got)
		}
	}
	// stdout is the result JSON the backend parses, not log output.
	if strings.Contains(got, `"outcome"`) {
		t.Errorf("the CLI's result JSON reached the job log:\n%s", got)
	}
}

func TestRebuildContextTeesAndRemovesTheOldContainer(t *testing.T) {
	argvLog := fakeDevcontainerCLI(t, 0)
	var jobLog bytes.Buffer
	ctx := backend.WithJobLog(context.Background(), &jobLog)

	if _, err := New().RebuildContext(ctx, "old-cid", backend.UpRequest{WorkspaceDir: t.TempDir()}); err != nil {
		t.Fatalf("RebuildContext: %v", err)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--remove-existing-container\n") {
		t.Errorf("rebuild did not pass --remove-existing-container:\n%s", argv)
	}
	if !strings.Contains(jobLog.String(), "Container started") {
		t.Errorf("rebuild did not tee the CLI log:\n%s", jobLog.String())
	}
}

func TestUpFailureRedactsEnvValues(t *testing.T) {
	fakeDevcontainerCLI(t, 1)

	_, err := New().Up(t.TempDir(), nil)
	if err == nil {
		t.Fatal("Up succeeded with a failing CLI")
	}
	msg := err.Error()
	if strings.Contains(msg, "hunter2-secret") {
		t.Fatalf("failure detail leaks an -e value:\n%s", msg)
	}
	for _, want := range []string{"devcontainer up failed", "-e API_KEY=<redacted>", `"containerEnv":{"API_KEY":"<redacted>"}`, "Error: docker run failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure detail is missing %q:\n%s", want, msg)
		}
	}
}

// The devcontainer backend is ctx-aware for both provisioning entry points.
var (
	_ backend.UpContexter      = (*DevcontainerBackend)(nil)
	_ backend.RebuildContexter = (*DevcontainerBackend)(nil)
)
