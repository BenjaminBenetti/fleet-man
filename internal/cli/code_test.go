package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/editorlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
)

// codeDaemon answers ResolveEditorLaunch with reply.
type codeDaemon struct {
	fleetgrpc.FleetServiceClient
	reply *fleetgrpc.ResolveEditorLaunchReply
	asked *fleetgrpc.ResolveEditorLaunchRequest
}

func (d *codeDaemon) ResolveEditorLaunch(_ context.Context, req *fleetgrpc.ResolveEditorLaunchRequest, _ ...grpc.CallOption) (*fleetgrpc.ResolveEditorLaunchReply, error) {
	d.asked = req
	return d.reply, nil
}

// codeRig points `fleet code` at a daemon answering reply, in a home of its
// own, with a `code` stub on PATH that records each run's arguments (and
// lists the Remote-SSH extension without recording).
func codeRig(t *testing.T, reply *fleetgrpc.ResolveEditorLaunchReply) (d *codeDaemon, home, argsFile string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer, fleetclient.EnvToken} {
		t.Setenv(name, "")
	}
	d = &codeDaemon{reply: reply}
	oldDial, oldLocal, oldInteractive := codeDial, codeLocal, codeInteractive
	codeDial = func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) { return d, func() {}, nil }
	codeLocal = func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
		t.Fatal("the local daemon was dialled for the local target")
		return nil, nil, nil
	}
	codeInteractive = func() bool { return false }
	t.Cleanup(func() { codeDial, codeLocal, codeInteractive = oldDial, oldLocal, oldInteractive })

	bin := t.TempDir()
	argsFile = filepath.Join(bin, "args")
	stub := "#!/bin/sh\n" +
		"if [ \"$1\" = --list-extensions ]; then echo ms-vscode-remote.remote-ssh; exit 0; fi\n" +
		"for a in \"$@\"; do echo \"$a\"; done > " + argsFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "code"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return d, home, argsFile
}

func devURIReply() *fleetgrpc.ResolveEditorLaunchReply {
	return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI,
		Argv: []string{"code", "--folder-uri", "vscode-remote://dev-container+2f77732f776562/workspaces/web"}}
}

func runCode(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newCodeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestCodePrintPrintsTheEditorCommandWithoutRunningIt(t *testing.T) {
	d, _, argsFile := codeRig(t, devURIReply())
	out, _, err := runCode(t, "--print", "web/alpha")
	if err != nil {
		t.Fatalf("code --print: %v", err)
	}
	if want := "code --folder-uri 'vscode-remote://dev-container+2f77732f776562/workspaces/web'\n"; out != want {
		t.Fatalf("code --print printed %q, want %q", out, want)
	}
	if d.asked.GetFleet() != "web" || d.asked.GetInstance() != "alpha" {
		t.Fatalf("asked the daemon about %v", d.asked)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("code --print ran the editor")
	}
}

func TestCodeRunsThePrintedCommand(t *testing.T) {
	_, _, argsFile := codeRig(t, devURIReply())
	if _, _, err := runCode(t, "web/alpha"); err != nil {
		t.Fatalf("code: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the editor was not run: %v", err)
	}
	want := []string{"--folder-uri", "vscode-remote://dev-container+2f77732f776562/workspaces/web"}
	if got := strings.Fields(string(data)); !slices.Equal(got, want) {
		t.Fatalf("editor args = %q, want %q", got, want)
	}
}

// Remote-SSH: --print gives the alias's URI and writes fleet_config, and
// says the Include is missing; without a terminal `fleet code` adds nothing
// and runs nothing, naming the manual steps; once the Include is there it
// opens the alias.
func TestCodeOverRemoteSSH(t *testing.T) {
	_, home, argsFile := codeRig(t, &fleetgrpc.ResolveEditorLaunchReply{
		Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH, WorkspaceFolder: "/workspaces/web"})
	out, errOut, err := runCode(t, "--print", "web/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if want := "code --folder-uri 'vscode-remote://ssh-remote+alpha.web.fleet/workspaces/web'\n"; out != want {
		t.Fatalf("printed %q, want %q", out, want)
	}
	if !strings.Contains(errOut, sshalias.IncludeLine) {
		t.Errorf("no note about the Include: %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "fleet_config")); err != nil {
		t.Fatalf("fleet_config: %v", err)
	}

	_, _, err = runCode(t, "web/alpha")
	if err == nil || !strings.Contains(err.Error(), "Include fleet_config") {
		t.Fatalf("without consent: %v", err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("the editor ran without the Include")
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "config")); err == nil {
		t.Fatal("~/.ssh/config was written without consent")
	}

	// Consent on a terminal.
	codeInteractive = func() bool { return true }
	cmd := newCodeCmd()
	var errBuf bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs([]string{"web/alpha"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "Fleet will add one line to ~/.ssh/config") {
		t.Errorf("no consent prompt: %q", errBuf.String())
	}
	if has, _ := editorlaunch.IncludePresent(); !has {
		t.Fatal("the Include was not added")
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !slices.Equal(got, []string{"--folder-uri", "vscode-remote://ssh-remote+alpha.web.fleet/workspaces/web"}) {
		t.Fatalf("editor args %q", got)
	}
}

func TestCodeConsentRefused(t *testing.T) {
	_, home, argsFile := codeRig(t, &fleetgrpc.ResolveEditorLaunchReply{
		Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH, WorkspaceFolder: "/workspaces/web"})
	codeInteractive = func() bool { return true }
	cmd := newCodeCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetArgs([]string{"web/alpha"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("refused consent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "config")); err == nil {
		t.Fatal("~/.ssh/config written after a no")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("the editor ran after a no")
	}
}

// The ssh server has no way to listen: its only connection is its stdio.
func TestSSHServerIsStdioOnly(t *testing.T) {
	cmd := newSSHServerCmd()
	var flags []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { flags = append(flags, f.Name) })
	if !slices.Equal(flags, []string{"stdio"}) {
		t.Fatalf("ssh-server flags %q, want only --stdio", flags)
	}
	cmd.SetArgs(nil)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--stdio") {
		t.Fatalf("ssh-server without --stdio: %v", err)
	}
	if !cmd.Hidden || !newSSHProxyCmd().Hidden {
		t.Fatal("the ssh plumbing commands are not hidden")
	}
}

// ssh-proxy refuses an alias outside the grammar before dialling anything.
func TestSSHProxyRefusesABadAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, alias := range []string{"", "alpha.web", "alpha.web.fleet.evil.com", "$(id).web.fleet", "alpha.web.rZZZZZZ.fleet"} {
		if _, err := runSSHProxy(context.Background(), alias, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("ssh-proxy took %q", alias)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".fleet")); err == nil {
		t.Error("a refused alias reached a daemon")
	}
}
