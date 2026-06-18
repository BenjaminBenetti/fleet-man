package devcontainer

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

func TestWithIsolatedTmp(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{name: "replaces inherited TMPDIR", env: []string{"PATH=/bin", "TMPDIR=/tmp", "HOME=/root"}},
		{name: "appends when none inherited", env: []string{"PATH=/bin", "HOME=/root"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withIsolatedTmp(tt.env, "/scratch/x")

			if slices.Contains(got, "TMPDIR=/tmp") {
				t.Fatalf("withIsolatedTmp kept inherited TMPDIR: %#v", got)
			}
			if !slices.Contains(got, "TMPDIR=/scratch/x") {
				t.Fatalf("withIsolatedTmp missing override TMPDIR: %#v", got)
			}
			if !slices.Contains(got, "PATH=/bin") || !slices.Contains(got, "HOME=/root") {
				t.Fatalf("withIsolatedTmp dropped unrelated env: %#v", got)
			}

			// Exactly one TMPDIR survives so the override is unambiguous.
			n := 0
			for _, e := range got {
				if strings.HasPrefix(e, "TMPDIR=") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("withIsolatedTmp produced %d TMPDIR entries, want 1: %#v", n, got)
			}
		})
	}
}

func TestScreenCaptureZeroValue(t *testing.T) {
	// A zero-value ScreenCapture should indicate failure (OK=false).
	var sc backend.ScreenCapture
	if sc.OK {
		t.Fatal("zero-value ScreenCapture should have OK=false")
	}
	if sc.Content != "" {
		t.Fatal("zero-value ScreenCapture should have empty Content")
	}
}

func TestParseToolProbeOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantTool string
		wantOK   bool
	}{
		{"claude detected", "claude\n", "claude", true},
		{"copilot detected", "copilot\n", "copilot", true},
		{"codex detected", "codex\n", "codex", true},
		{"gemini detected", "gemini\n", "gemini", true},
		{"auggie detected", "auggie\n", "auggie", true},
		{"no agent", "-\n", "", true},
		{"empty output", "", "", false},
		{"whitespace only", "  \n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := backend.ParseToolProbeOutput(tt.output)
			if ok != tt.wantOK {
				t.Fatalf("ParseToolProbeOutput(%q) ok = %v, want %v", tt.output, ok, tt.wantOK)
			}
			if tool != tt.wantTool {
				t.Fatalf("ParseToolProbeOutput(%q) tool = %q, want %q", tt.output, tool, tt.wantTool)
			}
		})
	}
}

func TestDevcontainerEnvBuildKitMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantEnv string
		wantErr bool
	}{
		{name: "unset delegates to default"},
		{name: "auto delegates to default", mode: "auto"},
		{name: "never disables buildkit", mode: "never", wantEnv: "DOCKER_BUILDKIT=0"},
		{name: "invalid errors", mode: "sometimes", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLEET_DEVCONTAINER_BUILDKIT", tt.mode)
			env, err := devcontainerEnv([]string{"PATH=/bin"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("devcontainerEnv() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("devcontainerEnv() error = %v", err)
			}
			if tt.wantEnv != "" && !slices.Contains(env, tt.wantEnv) {
				t.Fatalf("devcontainerEnv() missing %q in %#v", tt.wantEnv, env)
			}
			if tt.wantEnv == "" && slices.Contains(env, "DOCKER_BUILDKIT=0") {
				t.Fatalf("devcontainerEnv() unexpectedly disabled BuildKit: %#v", env)
			}
		})
	}
}

func TestDevcontainerUpArgsUpdateRemoteUserUID(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    []string
		wantErr bool
	}{
		{name: "unset delegates to default"},
		{name: "default delegates to default", mode: "default"},
		{name: "never disables uid rewrite", mode: "never", want: []string{"up", "--update-remote-user-uid-default", "never"}},
		{name: "on is passed through", mode: "on", want: []string{"up", "--update-remote-user-uid-default", "on"}},
		{name: "off is passed through", mode: "off", want: []string{"up", "--update-remote-user-uid-default", "off"}},
		{name: "invalid errors", mode: "sometimes", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID", tt.mode)
			got, err := devcontainerUpArgs([]string{"up"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("devcontainerUpArgs() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("devcontainerUpArgs() error = %v", err)
			}
			if tt.want == nil {
				tt.want = []string{"up"}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("devcontainerUpArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRunUserCommandsArgs(t *testing.T) {
	t.Run("without ssh agent", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "")
		got := runUserCommandsArgs("/ws/alpha")
		want := []string{"run-user-commands", "--workspace-folder", "/ws/alpha"}
		if !slices.Equal(got, want) {
			t.Fatalf("runUserCommandsArgs() = %#v, want %#v", got, want)
		}
	})

	t.Run("with ssh agent threads remote-env", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
		got := runUserCommandsArgs("/ws/alpha")
		want := []string{
			"run-user-commands", "--workspace-folder", "/ws/alpha",
			"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("runUserCommandsArgs() = %#v, want %#v", got, want)
		}
	})
}

func TestForwardStdioCommandArgv(t *testing.T) {
	cmd, ok := New().ForwardStdioCommand("cid123", 8080)
	if !ok {
		t.Fatalf("devcontainer backend should support a stdio bridge")
	}
	want := []string{"docker", "exec", "-i", "cid123", "socat", "STDIO", "TCP:localhost:8080"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("argv = %v, want %v", cmd.Args, want)
	}
}
