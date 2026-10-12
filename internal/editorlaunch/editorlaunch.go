// Package editorlaunch is the client half of opening VS Code on an instance,
// shared by the TUI's `c` and `fleet code`: it asks the daemon how
// (ResolveEditorLaunch), and for VS Code Remote-SSH builds the instance's
// `*.fleet` alias, keeps ~/.ssh/fleet_config, the Include line that pulls it
// into ~/.ssh/config (only with the user's consent), and the record that lets
// `fleet ssh-proxy` find a remote daemon again without the client's
// environment. It also resolves an alias back to a daemon for ssh-proxy.
//
// BOUNDARY: client code (depguard's client rule): it reaches the system only
// through fleetgrpc and fleetclient.
package editorlaunch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
)

// Launch is how to open VS Code on one instance.
type Launch struct {
	Mode fleetgrpc.EditorLaunchMode
	// Argv is the command to run.
	Argv []string
	// Alias is the instance's ssh host (REMOTE_SSH only).
	Alias string
	// NeedsInclude: ~/.ssh/config does not include fleet_config yet, so the
	// alias does not resolve until AddInclude runs (REMOTE_SSH only).
	NeedsInclude bool
}

// RemoteSSH reports whether the launch goes through VS Code Remote-SSH.
func (l Launch) RemoteSSH() bool {
	return l.Mode == fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH
}

// Services are the daemon connections a launch uses: the one the client
// works against (local or remote), and the local daemon's, which keeps the
// armada and the editor endpoint records.
type Services struct {
	Target fleetgrpc.FleetServiceClient
	// Local is dialled only when the target is a remote daemon.
	Local func(context.Context) (fleetgrpc.FleetServiceClient, func(), error)
}

// LocalDaemon dials the local daemon (fleetclient.DialLocal) for Services.
func LocalDaemon(ctx context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn.Service(), func() { _ = conn.Close() }, nil
}

// Executable is the fleet binary fleet_config's ProxyCommand runs (a var
// for tests).
var Executable = os.Executable

// Resolve works out how to open VS Code on instance in fleetName. For
// Remote-SSH it also writes ~/.ssh/fleet_config and, when the client points
// at a remote daemon the armada does not hold, records that remote on the
// local daemon, so the alias resolves from a shell without the client's
// environment. It never edits ~/.ssh/config (AddInclude does, on consent).
func Resolve(ctx context.Context, svcs Services, fleetName, instance string) (Launch, error) {
	reply, err := svcs.Target.ResolveEditorLaunch(ctx, &fleetgrpc.ResolveEditorLaunchRequest{Fleet: fleetName, Instance: instance})
	if err != nil {
		return Launch{}, err
	}
	l := Launch{Mode: reply.GetMode(), Argv: reply.GetArgv()}
	if !l.RemoteSSH() {
		if len(l.Argv) == 0 {
			return Launch{}, fmt.Errorf("the daemon gave no editor command for %s/%s", fleetName, instance)
		}
		return l, nil
	}
	remote := ""
	if r, ok := fleetclient.CurrentRemote(); ok {
		if remote, err = recordRemote(ctx, svcs, r); err != nil {
			return Launch{}, err
		}
	}
	if err := WriteConfig(); err != nil {
		return Launch{}, err
	}
	l.Alias = sshalias.Alias(fleetName, instance, remote)
	l.Argv = []string{"code", "--folder-uri", sshalias.URI(l.Alias, reply.GetWorkspaceFolder())}
	has, err := IncludePresent()
	if err != nil {
		return Launch{}, err
	}
	l.NeedsInclude = !has
	return l, nil
}

// remoteKey is what an alias's remote part hashes for r.
func remoteKey(r fleetclient.Remote) string {
	return sshalias.RemoteKey(r.Kind == fleetclient.RemoteServer, r.Address)
}

// recordRemote makes sure ssh-proxy can find r again by its alias's remote
// part: an armada remote it finds there; any other is recorded on the local
// daemon (SaveEditorEndpoint, with r's explicit token).
func recordRemote(ctx context.Context, svcs Services, r fleetclient.Remote) (string, error) {
	remote := sshalias.RemoteHash(remoteKey(r))
	local, done, err := svcs.Local(ctx)
	if err != nil {
		return "", fmt.Errorf("reach the local fleet daemon to record the remote: %w", err)
	}
	defer done()
	if r.Kind != fleetclient.RemoteServer {
		armada, err := local.GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
		if err != nil {
			return "", fmt.Errorf("read the armada: %w", err)
		}
		for _, a := range armada.GetRemotes() {
			if strings.TrimSpace(a.GetUrl()) == r.Address {
				return remote, nil
			}
		}
	}
	reply, err := local.SaveEditorEndpoint(ctx, &fleetgrpc.SaveEditorEndpointRequest{Endpoint: &fleetgrpc.EditorEndpoint{
		Kind: endpointKind(r.Kind), Address: r.Address, Token: r.Token,
	}})
	if err != nil {
		return "", fmt.Errorf("record the remote for ssh-proxy: %w", err)
	}
	if reply.GetRemote() != remote {
		return "", fmt.Errorf("the local daemon recorded the remote as %q, not %q (is it another fleet version?)", reply.GetRemote(), remote)
	}
	return remote, nil
}

func endpointKind(k fleetclient.RemoteKind) fleetgrpc.EditorEndpointKind {
	switch k {
	case fleetclient.RemoteGateway:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY
	case fleetclient.RemoteSSH:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SSH
	case fleetclient.RemoteServer:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER
	}
	return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_UNSPECIFIED
}

func remoteKind(k fleetgrpc.EditorEndpointKind) (fleetclient.RemoteKind, bool) {
	switch k {
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY:
		return fleetclient.RemoteGateway, true
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SSH:
		return fleetclient.RemoteSSH, true
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER:
		return fleetclient.RemoteServer, true
	}
	return "", false
}

// sshDir is ~/.ssh.
func sshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh"), nil
}

// ConfigPath is ~/.ssh/fleet_config.
func ConfigPath() (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sshalias.ConfigFileName), nil
}

// UserConfigPath is ~/.ssh/config.
func UserConfigPath() (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// WriteConfig keeps ~/.ssh/fleet_config (0600) routing *.fleet through
// this fleet binary's ssh-proxy, rewriting it only when it differs.
func WriteConfig() error {
	exe, err := Executable()
	if err != nil {
		return fmt.Errorf("find the fleet binary: %w", err)
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	want := sshalias.Config(exe)
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ~/.ssh: %w", err)
	}
	if err := writeAtomic(path, []byte(want), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// IncludePresent reports whether ~/.ssh/config includes fleet_config.
func IncludePresent() (bool, error) {
	path, err := UserConfigPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sshalias.HasInclude(string(data)), nil
}

// AddInclude adds `Include fleet_config` at the top of ~/.ssh/config
// (creating it 0600), where it applies to every host. A config that is a
// link (a dotfiles checkout) is edited where it points. The user's consent
// comes first: the caller asks.
func AddInclude() error {
	path, err := UserConfigPath()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := fs.FileMode(0o600)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create ~/.ssh: %w", err)
		}
	case err != nil:
		return err
	default:
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	next := sshalias.WithInclude(string(data))
	if next == string(data) {
		return nil
	}
	if err := writeAtomic(path, []byte(next), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeAtomic replaces path with data (temp file + rename), with mode.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ManualSteps is what to do by hand instead of AddInclude.
func ManualSteps(l Launch) string {
	return "Add this line at the top of ~/.ssh/config:\n" +
		"    " + sshalias.IncludeLine + "\n" +
		"(~/.ssh/" + sshalias.ConfigFileName + " routes hosts " + sshalias.HostPattern + " through `fleet " + sshalias.ProxySubcommand + "`.)\n" +
		"Then open the instance with:\n" +
		"    " + CommandLine(l.Argv)
}

// CommandLine joins argv into one line a POSIX shell runs as the same
// command, quoting only the words that need it.
func CommandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, word := range argv {
		words[i] = shellquote.CommandWord(word)
	}
	return strings.Join(words, " ")
}

// RemoteSSHExtensions are the extension ids that give VS Code Remote-SSH
// (Microsoft's, and Open Remote - SSH for VS Code builds without it).
var RemoteSSHExtensions = []string{"ms-vscode-remote.remote-ssh", "jeanp413.open-remote-ssh"}

// listExtensions runs `<code> --list-extensions` (a var for tests).
var listExtensions = func(code string) ([]byte, error) {
	return exec.Command(code, "--list-extensions").Output()
}

// CheckRemoteSSH checks the VS Code CLI (argv[0] of a launch) has a
// Remote-SSH extension, naming the install command when it does not.
func CheckRemoteSSH(code string) error {
	out, err := listExtensions(code)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("could not list VS Code's extensions with `%s --list-extensions`: %w", code, err)
		}
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.ToLower(strings.TrimSpace(line))
		if i := strings.IndexByte(id, '@'); i >= 0 {
			id = id[:i]
		}
		for _, want := range RemoteSSHExtensions {
			if id == want {
				return nil
			}
		}
	}
	return fmt.Errorf("VS Code's Remote - SSH extension is not installed: run `%s --install-extension %s`", code, RemoteSSHExtensions[0])
}

// Dialer connects to the daemon an alias leads to.
type Dialer func(ctx context.Context) (*fleetclient.Conn, error)

// ResolveAlias reads an alias strictly and finds the daemon it leads to,
// without the client's environment: the local daemon for an alias with no
// remote part; else the armada remote, or the recorded editor endpoint,
// whose hash the remote part is. Nothing is dialled for an alias that does
// not parse.
func ResolveAlias(ctx context.Context, alias string, local func(context.Context) (fleetgrpc.FleetServiceClient, func(), error)) (sshalias.Target, Dialer, error) {
	target, dial, _, err := ResolveAliasRemote(ctx, alias, local)
	return target, dial, err
}

// ResolveAliasRemote is ResolveAlias that also says which armada remote the
// alias leads to: its gateway or ssh:// URL, "" for the local daemon or one
// reached by a FLEET_SERVER address (no armada remote is either).
func ResolveAliasRemote(ctx context.Context, alias string, local func(context.Context) (fleetgrpc.FleetServiceClient, func(), error)) (sshalias.Target, Dialer, string, error) {
	target, err := sshalias.Parse(alias)
	if err != nil {
		return sshalias.Target{}, nil, "", err
	}
	if target.Remote == "" {
		return target, fleetclient.DialLocal, "", nil
	}
	svc, done, err := local(ctx)
	if err != nil {
		return sshalias.Target{}, nil, "", fmt.Errorf("reach the local fleet daemon: %w", err)
	}
	defer done()
	r, err := findRemote(ctx, svc, target.Remote)
	if err != nil {
		return sshalias.Target{}, nil, "", err
	}
	url := ""
	if r.Kind == fleetclient.RemoteGateway || r.Kind == fleetclient.RemoteSSH {
		url = r.Address
	}
	return target, func(ctx context.Context) (*fleetclient.Conn, error) { return fleetclient.DialRemote(ctx, r) }, url, nil
}

// findRemote is the remote daemon whose hash remote is.
func findRemote(ctx context.Context, svc fleetgrpc.FleetServiceClient, remote string) (fleetclient.Remote, error) {
	armada, err := svc.GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		return fleetclient.Remote{}, fmt.Errorf("read the armada: %w", err)
	}
	for _, a := range armada.GetRemotes() {
		url := strings.TrimSpace(a.GetUrl())
		if url == "" || sshalias.RemoteHash(url) != remote {
			continue
		}
		if fleetclient.IsSSHURL(url) {
			return fleetclient.Remote{Kind: fleetclient.RemoteSSH, Address: url}, nil
		}
		return fleetclient.Remote{Kind: fleetclient.RemoteGateway, Address: url, Token: a.GetToken()}, nil
	}
	reply, err := svc.ResolveEditorEndpoint(ctx, &fleetgrpc.ResolveEditorEndpointRequest{Remote: remote})
	if err != nil {
		return fleetclient.Remote{}, fmt.Errorf("look up the remote: %w", err)
	}
	ep := reply.GetEndpoint()
	kind, ok := remoteKind(ep.GetKind())
	if ep == nil || !ok || ep.GetAddress() == "" {
		return fleetclient.Remote{}, fmt.Errorf("no remote fleet daemon is known by %s here: open the instance with `fleet code` (or `c` in the TUI) while connected to it, or add the remote to the armada", remote)
	}
	return fleetclient.Remote{Kind: kind, Address: ep.GetAddress(), Token: ep.GetToken()}, nil
}
