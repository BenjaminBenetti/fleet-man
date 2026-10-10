package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestEditorArgv(t *testing.T) {
	const ws = "/home/me/.fleet/workspaces/web/alpha/web"
	devURI := "vscode-remote://dev-container+" + hex.EncodeToString([]byte(ws)) + "/workspaces/web"
	for _, c := range []struct {
		backend fleet.BackendType
		want    []string
	}{
		{"", []string{"code", "--folder-uri", devURI}},
		{fleet.BackendDevcontainer, []string{"code", "--folder-uri", devURI}},
		{fleet.BackendCoder, []string{"coder", "open", "vscode", "ws-id"}},
		{fleet.BackendCodespaces, []string{"gh", "codespace", "code", "-c", "ws-id"}},
	} {
		inst := &fleet.Instance{Name: "alpha", Backend: c.backend, ContainerID: "ws-id", WorkspaceDir: ws}
		if got := editorArgv("web", inst); !slices.Equal(got, c.want) {
			t.Errorf("editorArgv(%q) = %q, want %q", c.backend, got, c.want)
		}
	}
}

func TestCommandLineQuotesOnlyWordsThatNeedIt(t *testing.T) {
	got := commandLine([]string{"code", "--folder-uri", "vscode-remote://dev-container+2f/workspaces/x", "it's"})
	want := `code --folder-uri 'vscode-remote://dev-container+2f/workspaces/x' 'it'\''s'`
	if got != want {
		t.Fatalf("commandLine = %s, want %s", got, want)
	}
}

// seedCodeInstance writes a one-instance devcontainer fleet to a temp HOME and
// puts a `code` stub that records its arguments first on PATH.
func seedCodeInstance(t *testing.T) (ref, workspaceDir, argsFile string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspaceDir = filepath.Join(home, ".fleet", "workspaces", "web", "alpha", "web")
	st := &state.State{Fleets: map[string]*fleet.Fleet{"web": {
		Name: "web",
		Instances: []*fleet.Instance{{
			Name: "alpha", ContainerID: "cid", WorkspaceDir: workspaceDir,
			CreatedAt: time.Unix(0, 0), Status: fleet.StatusRunning,
		}},
	}}}
	if err := state.Save(st); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	bin := t.TempDir()
	argsFile = filepath.Join(bin, "args")
	stub := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done > " + argsFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "code"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return "web/alpha", workspaceDir, argsFile
}

func TestCodePrintPrintsTheEditorCommandWithoutRunningIt(t *testing.T) {
	ref, ws, argsFile := seedCodeInstance(t)
	cmd := newCodeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--print", ref})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("code --print: %v", err)
	}
	want := "code --folder-uri 'vscode-remote://dev-container+" + hex.EncodeToString([]byte(ws)) + "/workspaces/web'\n"
	if out.String() != want {
		t.Fatalf("code --print printed %q, want %q", out.String(), want)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("code --print ran the editor")
	}
}

func TestCodeRunsThePrintedCommand(t *testing.T) {
	ref, ws, argsFile := seedCodeInstance(t)
	cmd := newCodeCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{ref})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("code: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the editor was not run: %v", err)
	}
	want := []string{"--folder-uri", "vscode-remote://dev-container+" + hex.EncodeToString([]byte(ws)) + "/workspaces/web"}
	if got := strings.Fields(string(data)); !slices.Equal(got, want) {
		t.Fatalf("editor args = %q, want %q", got, want)
	}
}
