package editorlaunch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"google.golang.org/grpc"
)

// fakeDaemon answers the editor RPCs from fields and records the endpoints
// saved on it.
type fakeDaemon struct {
	fleetgrpc.FleetServiceClient
	launch  *fleetgrpc.ResolveEditorLaunchReply
	armada  []*fleetgrpc.ArmadaRemote
	records map[string]*fleetgrpc.EditorEndpoint
	saved   []*fleetgrpc.EditorEndpoint
}

func (f *fakeDaemon) ResolveEditorLaunch(context.Context, *fleetgrpc.ResolveEditorLaunchRequest, ...grpc.CallOption) (*fleetgrpc.ResolveEditorLaunchReply, error) {
	return f.launch, nil
}

func (f *fakeDaemon) GetArmada(context.Context, *fleetgrpc.GetArmadaRequest, ...grpc.CallOption) (*fleetgrpc.GetArmadaReply, error) {
	return &fleetgrpc.GetArmadaReply{Remotes: f.armada}, nil
}

func (f *fakeDaemon) SaveEditorEndpoint(_ context.Context, req *fleetgrpc.SaveEditorEndpointRequest, _ ...grpc.CallOption) (*fleetgrpc.SaveEditorEndpointReply, error) {
	f.saved = append(f.saved, req.GetEndpoint())
	key := sshalias.RemoteKey(req.GetEndpoint().GetKind() == fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER, req.GetEndpoint().GetAddress())
	return &fleetgrpc.SaveEditorEndpointReply{Remote: sshalias.RemoteHash(key)}, nil
}

func (f *fakeDaemon) ResolveEditorEndpoint(_ context.Context, req *fleetgrpc.ResolveEditorEndpointRequest, _ ...grpc.CallOption) (*fleetgrpc.ResolveEditorEndpointReply, error) {
	return &fleetgrpc.ResolveEditorEndpointReply{Endpoint: f.records[req.GetRemote()]}, nil
}

func (f *fakeDaemon) local() func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
	return func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) { return f, func() {}, nil }
}

// clientEnv is a home of its own and no remote endpoint variables.
func clientEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer, fleetclient.EnvToken} {
		t.Setenv(name, "")
	}
	old := Executable
	Executable = func() (string, error) { return "/opt/fleet/bin/fleet", nil }
	t.Cleanup(func() { Executable = old })
	return home
}

func remoteSSHReply() *fleetgrpc.ResolveEditorLaunchReply {
	return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH, WorkspaceFolder: "/workspaces/repo"}
}

func TestResolveRemoteSSHOnTheLocalDaemon(t *testing.T) {
	home := clientEnv(t)
	d := &fakeDaemon{launch: remoteSSHReply()}
	l, err := Resolve(context.Background(), Services{Target: d, Local: func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
		t.Fatal("the local daemon was dialled for a local target")
		return nil, nil, nil
	}}, "repo", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !l.RemoteSSH() || l.Alias != "alpha.repo.fleet" || !l.NeedsInclude {
		t.Fatalf("launch %+v", l)
	}
	if want := []string{"code", "--folder-uri", "vscode-remote://ssh-remote+alpha.repo.fleet/workspaces/repo"}; !slices.Equal(l.Argv, want) {
		t.Fatalf("argv %q, want %q", l.Argv, want)
	}
	cfg := filepath.Join(home, ".ssh", "fleet_config")
	data, err := os.ReadFile(cfg)
	if err != nil || string(data) != sshalias.Config("/opt/fleet/bin/fleet") {
		t.Fatalf("fleet_config %q, %v", data, err)
	}
	if info, _ := os.Stat(cfg); info.Mode().Perm() != 0o600 {
		t.Errorf("fleet_config mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(home, ".ssh")); info.Mode().Perm() != 0o700 {
		t.Errorf("~/.ssh mode %v", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Resolve touched ~/.ssh/config without consent")
	}
	if err := AddInclude(); err != nil {
		t.Fatal(err)
	}
	if l, _ := Resolve(context.Background(), Services{Target: d}, "repo", "alpha"); l.NeedsInclude {
		t.Error("the Include is still missing after AddInclude")
	}
}

// A remote chosen by the variables is recorded on the local daemon, with the
// token only when FLEET_TOKEN gave one; an armada remote is not recorded.
func TestResolveRecordsAnEnvRemote(t *testing.T) {
	clientEnv(t)
	gw := "https://gw.example/grpc/s1"
	t.Setenv(fleetclient.EnvGateway, gw)
	d := &fakeDaemon{launch: remoteSSHReply()}
	l, err := Resolve(context.Background(), Services{Target: d, Local: d.local()}, "repo", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	remote := sshalias.RemoteHash(gw)
	if l.Alias != "alpha.repo."+remote+".fleet" || !strings.Contains(l.Argv[2], "ssh-remote+alpha.repo."+remote+".fleet/") {
		t.Fatalf("launch %+v", l)
	}
	if len(d.saved) != 1 || d.saved[0].GetAddress() != gw || d.saved[0].GetToken() != "" ||
		d.saved[0].GetKind() != fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY {
		t.Fatalf("saved %v", d.saved)
	}
	data, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "fleet_config"))
	if strings.Contains(string(data), "tok") {
		t.Fatal("a token reached fleet_config")
	}

	t.Setenv(fleetclient.EnvToken, "tok")
	d.saved = nil
	if _, err := Resolve(context.Background(), Services{Target: d, Local: d.local()}, "repo", "alpha"); err != nil {
		t.Fatal(err)
	}
	if len(d.saved) != 1 || d.saved[0].GetToken() != "tok" {
		t.Fatalf("saved %v", d.saved)
	}
	data, _ = os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "fleet_config"))
	if strings.Contains(string(data), "tok") {
		t.Fatal("a token reached fleet_config")
	}

	d.saved = nil
	d.armada = []*fleetgrpc.ArmadaRemote{{Url: gw, Token: "tok"}}
	if _, err := Resolve(context.Background(), Services{Target: d, Local: d.local()}, "repo", "alpha"); err != nil {
		t.Fatal(err)
	}
	if len(d.saved) != 0 {
		t.Fatalf("an armada remote was recorded: %v", d.saved)
	}

	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvServer, "10.0.0.1:7777")
	l, err = Resolve(context.Background(), Services{Target: d, Local: d.local()}, "repo", "alpha")
	if err != nil || l.Alias != "alpha.repo."+sshalias.RemoteHash("server:10.0.0.1:7777")+".fleet" {
		t.Fatalf("server remote: %+v %v", l, err)
	}
}

func TestResolvePassesTheCLIModesThrough(t *testing.T) {
	home := clientEnv(t)
	argv := []string{"coder", "open", "vscode", "ws"}
	d := &fakeDaemon{launch: &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODER_CLI, Argv: argv}}
	l, err := Resolve(context.Background(), Services{Target: d}, "repo", "alpha")
	if err != nil || l.RemoteSSH() || !slices.Equal(l.Argv, argv) || l.NeedsInclude {
		t.Fatalf("launch %+v %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a CLI launch wrote ssh config")
	}
	d.launch = &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI}
	if _, err := Resolve(context.Background(), Services{Target: d}, "repo", "alpha"); err == nil {
		t.Error("a launch with no command was taken")
	}
}

func TestAddIncludeKeepsTheUsersConfig(t *testing.T) {
	home := clientEnv(t)
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A dotfiles-managed config: ~/.ssh/config links to it.
	real := filepath.Join(home, "dotfiles-ssh-config")
	if err := os.WriteFile(real, []byte("Host box\n  User me\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := AddInclude(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(real)
	if string(data) != "Include fleet_config\n\nHost box\n  User me\n" {
		t.Fatalf("config %q", data)
	}
	if info, _ := os.Lstat(filepath.Join(dir, "config")); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	if err := AddInclude(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(real); string(again) != string(data) {
		t.Error("a second AddInclude changed the config")
	}
	if has, err := IncludePresent(); err != nil || !has {
		t.Errorf("IncludePresent = %v, %v", has, err)
	}
}

func TestManualSteps(t *testing.T) {
	steps := ManualSteps(Launch{Argv: []string{"code", "--folder-uri", "vscode-remote://ssh-remote+a.b.fleet/w"}})
	for _, want := range []string{"Include fleet_config", "~/.ssh/config", "code --folder-uri 'vscode-remote://ssh-remote+a.b.fleet/w'"} {
		if !strings.Contains(steps, want) {
			t.Errorf("manual steps lack %q:\n%s", want, steps)
		}
	}
}

func TestCheckRemoteSSH(t *testing.T) {
	old := listExtensions
	t.Cleanup(func() { listExtensions = old })
	for _, tc := range []struct {
		out string
		err error
		ok  bool
	}{
		{"ms-python.python\nms-vscode-remote.remote-ssh\n", nil, true},
		{"MS-VSCode-Remote.Remote-SSH@0.110.0\n", nil, true},
		{"jeanp413.open-remote-ssh\n", nil, true},
		{"ms-vscode-remote.remote-containers\n", nil, false},
		{"", errors.New("boom"), false},
	} {
		listExtensions = func(string) ([]byte, error) { return []byte(tc.out), tc.err }
		err := CheckRemoteSSH("code")
		if (err == nil) != tc.ok {
			t.Errorf("CheckRemoteSSH with %q, %v: %v", tc.out, tc.err, err)
		}
		if !tc.ok && tc.err == nil && !strings.Contains(err.Error(), "--install-extension ms-vscode-remote.remote-ssh") {
			t.Errorf("no install hint: %v", err)
		}
	}
}

// ssh-proxy's view: an alias parses before anything is dialled, an alias
// without a remote part is the local daemon, a remote part is looked up in
// the armada first and then in the records.
func TestResolveAlias(t *testing.T) {
	clientEnv(t)
	ctx := context.Background()
	never := func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
		t.Fatal("the local daemon was dialled")
		return nil, nil, nil
	}
	if _, _, err := ResolveAlias(ctx, "alpha;rm -rf.repo.fleet", never); err == nil {
		t.Fatal("a bad alias was taken")
	}
	target, dial, err := ResolveAlias(ctx, "alpha.repo.fleet", never)
	if err != nil || target != (sshalias.Target{Fleet: "repo", Instance: "alpha"}) || dial == nil {
		t.Fatalf("local alias: %+v %v", target, err)
	}

	gw := "https://gw.example/grpc/s1"
	sshURL := "ssh://me@box"
	d := &fakeDaemon{
		armada: []*fleetgrpc.ArmadaRemote{{Url: sshURL}},
		records: map[string]*fleetgrpc.EditorEndpoint{
			sshalias.RemoteHash(gw): {Kind: fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY, Address: gw, Token: "tok"},
		},
	}
	for _, tc := range []struct {
		remote string
		want   fleetclient.Remote
	}{
		{sshalias.RemoteHash(sshURL), fleetclient.Remote{Kind: fleetclient.RemoteSSH, Address: sshURL}},
		{sshalias.RemoteHash(gw), fleetclient.Remote{Kind: fleetclient.RemoteGateway, Address: gw, Token: "tok"}},
	} {
		got, err := findRemote(ctx, d, tc.remote)
		if err != nil || got != tc.want {
			t.Errorf("findRemote(%s) = %+v, %v; want %+v", tc.remote, got, err, tc.want)
		}
		if _, dial, err := ResolveAlias(ctx, "alpha.repo."+tc.remote+".fleet", d.local()); err != nil || dial == nil {
			t.Errorf("ResolveAlias with %s: %v", tc.remote, err)
		}
		// The remote's URL, which its armada entry (agent forwarding) is
		// found by.
		if _, _, url, err := ResolveAliasRemote(ctx, "alpha.repo."+tc.remote+".fleet", d.local()); err != nil || url != tc.want.Address {
			t.Errorf("ResolveAliasRemote with %s: url %q, %v; want %q", tc.remote, url, err, tc.want.Address)
		}
	}
	if _, _, url, _ := ResolveAliasRemote(ctx, "alpha.repo.fleet", never); url != "" {
		t.Errorf("the local daemon's alias names remote %q", url)
	}
	// The armada wins over a record with the same hash.
	d.armada = []*fleetgrpc.ArmadaRemote{{Url: gw, Token: "armada-tok"}}
	if got, _ := findRemote(ctx, d, sshalias.RemoteHash(gw)); got.Token != "armada-tok" {
		t.Errorf("record won over the armada: %+v", got)
	}
	if _, _, err := ResolveAlias(ctx, "alpha.repo.r000000.fleet", d.local()); err == nil || !strings.Contains(err.Error(), "fleet code") {
		t.Errorf("an unknown remote: %v", err)
	}
}

func TestCommandLineQuotesOnlyWordsThatNeedIt(t *testing.T) {
	got := CommandLine([]string{"code", "--folder-uri", "vscode-remote://dev-container+2f/workspaces/x", "it's"})
	want := `code --folder-uri 'vscode-remote://dev-container+2f/workspaces/x' 'it'\''s'`
	if got != want {
		t.Fatalf("CommandLine = %s, want %s", got, want)
	}
}
