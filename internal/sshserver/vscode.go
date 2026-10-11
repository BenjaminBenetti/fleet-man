package sshserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
)

// vscode.go applies the devcontainer's customizations for VS Code the way
// the Dev Containers extension does when it attaches, since over Remote-SSH
// nothing else does:
//
//   - Each connection watches for VS Code's server in the account's home
//     (~/.vscode-server, and -insiders: bin/<commit>/bin/code-server, or
//     cli/servers/<quality>-<commit>/server/bin/code-server for the exec
//     server Remote-SSH now installs). Once one is there, every extension in
//     the merged config is installed with its `--install-extension`, each
//     attempted once per container generation (EditorConfig.Gen), and the
//     merged settings, port attributes included, are merged into the
//     server's machine settings (data/Machine/settings.json) three ways:
//     against what fleet last wrote, so a value the user changed or a key
//     the user removed stays as the user left it. Connections share the work
//     under a lock, with what was done recorded in .fleet-customizations.json
//     beside the server.
//   - A connection is a VS Code connection once it forwards to an endpoint
//     VS Code's server (a process whose executable is in its directory)
//     listens on: that is how the editor reaches it. Then, once for that
//     connection, postAttachCommand runs as the account in the workspace
//     folder, as the Dev Containers extension runs it on every attach.
//
// Everything is reported on the server's log (the daemon's creation log).

// VSCodeOptions tune the customizations.
type VSCodeOptions struct {
	// Disabled turns them off.
	Disabled bool
	// Poll is how often a connection looks for VS Code's server (default 1s).
	Poll time.Duration
	// ConfigWait is how long a connection waits for its EditorConfig, which
	// the daemon may still be delivering (default 30s).
	ConfigWait time.Duration
	// InstallTimeout bounds one extension's install (default 5 minutes).
	InstallTimeout time.Duration
	// Owned reports whether a forward's endpoint (network "tcp" or "unix")
	// is listened on by a process whose executable is under one of dirs
	// (default: socketOwnedBy, from /proc; never off Linux).
	Owned func(network, address string, dirs []string) bool
}

func (o VSCodeOptions) withDefaults() VSCodeOptions {
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.ConfigWait <= 0 {
		o.ConfigWait = 30 * time.Second
	}
	if o.InstallTimeout <= 0 {
		o.InstallTimeout = 5 * time.Minute
	}
	if o.Owned == nil {
		o.Owned = socketOwnedBy
	}
	return o
}

// serverFlavour is one VS Code server install directory under the home.
type serverFlavour struct {
	dir string // e.g. .vscode-server
	bin string // its server's launcher, e.g. code-server
}

var serverFlavours = []serverFlavour{
	{".vscode-server", "code-server"},
	{".vscode-server-insiders", "code-server-insiders"},
}

// Files fleet keeps beside a server install.
const (
	customizationStateFile = ".fleet-customizations.json"
	customizationLockFile  = ".fleet-customizations.lock"
)

// vscodeWatch is one connection's VS Code customizations.
type vscodeWatch struct {
	c    *connection
	opts VSCodeOptions
	path string // the EditorConfig's

	wg sync.WaitGroup

	mu       sync.Mutex
	cfg      *devmeta.EditorConfig
	cfgErr   error
	cfgDone  bool
	applied  map[string]bool // server launchers this connection applied for
	attached bool            // postAttachCommand ran for this connection
	warned   bool            // the missing config was reported
}

func newVSCodeWatch(ctx context.Context, c *connection) *vscodeWatch {
	opts := VSCodeOptions{}
	if c.opts.VSCode != nil {
		opts = *c.opts.VSCode
	}
	w := &vscodeWatch{c: c, opts: opts.withDefaults(), path: c.opts.EditorConfig, applied: map[string]bool{}}
	if w.path == "" {
		w.path = devmeta.EditorConfigPath
	}
	if opts.Disabled {
		return w
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.watch(ctx)
	}()
	return w
}

// wait waits for the connection's customization work to end.
func (w *vscodeWatch) wait() { w.wg.Wait() }

// roots are the server install directories of the account's home.
func (w *vscodeWatch) roots() []serverFlavour {
	out := make([]serverFlavour, len(serverFlavours))
	for i, f := range serverFlavours {
		out[i] = serverFlavour{dir: filepath.Join(w.c.opts.User.Home, f.dir), bin: f.bin}
	}
	return out
}

// watch looks for VS Code's server until the connection ends.
func (w *vscodeWatch) watch(ctx context.Context) {
	t := time.NewTicker(w.opts.Poll)
	defer t.Stop()
	for {
		for _, root := range w.roots() {
			for _, server := range findServers(root) {
				w.mu.Lock()
				done := w.applied[server]
				w.applied[server] = true
				w.mu.Unlock()
				if !done {
					w.apply(ctx, root, server)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// findServers are the server launchers installed under root, newest first.
func findServers(root serverFlavour) []string {
	var found []string
	for _, pattern := range []string{
		filepath.Join(root.dir, "bin", "*", "bin", root.bin),
		filepath.Join(root.dir, "cli", "servers", "*", "server", "bin", root.bin),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				found = append(found, m)
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return modTime(found[i]).After(modTime(found[j])) })
	return found
}

func modTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// config is the connection's EditorConfig, read once (waiting up to
// ConfigWait for it to arrive).
func (w *vscodeWatch) config(ctx context.Context) (*devmeta.EditorConfig, error) {
	w.mu.Lock()
	if w.cfgDone {
		defer w.mu.Unlock()
		return w.cfg, w.cfgErr
	}
	w.mu.Unlock()
	deadline := time.Now().Add(w.opts.ConfigWait)
	var cfg *devmeta.EditorConfig
	var err error
	for {
		cfg, err = readEditorConfig(w.path)
		if err == nil || !errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cfgDone {
		w.cfg, w.cfgErr, w.cfgDone = cfg, err, true
	}
	return w.cfg, w.cfgErr
}

// configOrReport is config, reporting (once) why there is none.
func (w *vscodeWatch) configOrReport(ctx context.Context) *devmeta.EditorConfig {
	cfg, err := w.config(ctx)
	if err == nil {
		return cfg
	}
	if ctx.Err() != nil {
		return nil
	}
	w.mu.Lock()
	warned := w.warned
	w.warned = true
	w.mu.Unlock()
	if !warned {
		w.c.rep.printf("warning: vscode: no editor settings (%v): the devcontainer's VS Code customizations are not applied", err)
	}
	return nil
}

func readEditorConfig(path string) (*devmeta.EditorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg devmeta.EditorConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.V > devmeta.EditorConfigVersion {
		return nil, fmt.Errorf("%s is version %d; this fleet reads up to %d", path, cfg.V, devmeta.EditorConfigVersion)
	}
	return &cfg, nil
}

// customizationState is what fleet did for one server install directory.
type customizationState struct {
	V int `json:"v"`
	// Gen is the container generation the extension attempts are for.
	Gen string `json:"gen"`
	// Extensions are the attempted extensions (lower-case id) and how each
	// went: "installed", or the failure.
	Extensions map[string]string `json:"extensions,omitempty"`
	// Settings are the machine settings fleet last merged in: the base of
	// the next three-way merge.
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
}

// apply installs the extensions and merges the settings for one server.
func (w *vscodeWatch) apply(ctx context.Context, root serverFlavour, server string) {
	cfg := w.configOrReport(ctx)
	if cfg == nil {
		return
	}
	unlock, err := lockFile(ctx, filepath.Join(root.dir, customizationLockFile))
	if err != nil {
		if ctx.Err() == nil {
			w.c.rep.printf("warning: vscode: %v", err)
		}
		return
	}
	defer unlock()
	statePath := filepath.Join(root.dir, customizationStateFile)
	st := loadCustomizationState(statePath)
	if st.Gen != cfg.Gen {
		st.Gen, st.Extensions = cfg.Gen, nil
	}
	if st.Extensions == nil {
		st.Extensions = map[string]string{}
	}

	var pending []string
	for _, id := range cfg.Extensions {
		if _, tried := st.Extensions[strings.ToLower(id)]; !tried {
			pending = append(pending, id)
		}
	}
	if len(pending) > 0 {
		w.c.rep.printf("==> vscode: installing %d extension(s) with %s", len(pending), tildePath(server, w.c.opts.User.Home))
	}
	for _, id := range pending {
		if ctx.Err() != nil {
			return // the connection went; the next one installs the rest
		}
		result := w.install(ctx, server, id)
		st.Extensions[strings.ToLower(id)] = result
		saveCustomizationState(statePath, st)
	}

	settingsPath := filepath.Join(root.dir, "data", "Machine", "settings.json")
	changed, err := mergeMachineSettings(settingsPath, st.Settings, cfg.Settings)
	switch {
	case err != nil:
		w.c.rep.printf("warning: vscode machine settings: %v", err)
	default:
		if changed {
			w.c.rep.printf("==> vscode machine settings: %s updated (%d setting(s) from the devcontainer)", tildePath(settingsPath, w.c.opts.User.Home), len(cfg.Settings))
		}
		st.Settings = cfg.Settings
		saveCustomizationState(statePath, st)
	}
}

// install runs `<server> --install-extension id`, reporting how it went.
func (w *vscodeWatch) install(ctx context.Context, server, id string) string {
	ctx, cancel := context.WithTimeout(ctx, w.opts.InstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, server, "--install-extension", id)
	cmd.Env = w.env()
	cmd.Dir = w.c.opts.User.Home
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		w.c.rep.printf("==> vscode extension %s: installed", id)
		return "installed"
	}
	detail := lastLine(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("no answer in %s", w.opts.InstallTimeout)
	}
	msg := err.Error()
	if detail != "" {
		msg += ": " + detail
	}
	w.c.rep.printf("warning: vscode extension %s: %s", id, msg)
	return "failed: " + msg
}

// env is the environment commands fleet runs for VS Code get: a session's,
// without a terminal.
func (w *vscodeWatch) env() []string {
	return (&session{c: w.c}).environ()
}

// noteForward is called for each forward the connection opened: the first
// one to an endpoint VS Code's server owns makes this a VS Code connection,
// which runs postAttachCommand once.
func (w *vscodeWatch) noteForward(network, address string) {
	if w.opts.Disabled {
		return
	}
	w.mu.Lock()
	if w.attached {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	dirs := w.serverDirs()
	if len(dirs) == 0 {
		return // no VS Code server here, so no VS Code connection
	}
	if network == "tcp" {
		if !loopbackDial(address) {
			return
		}
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if !w.opts.Owned(network, address, dirs) {
			return
		}
		w.mu.Lock()
		if w.attached {
			w.mu.Unlock()
			return
		}
		w.attached = true
		w.mu.Unlock()
		w.postAttach(w.c.ctx)
	}()
}

// serverDirs are the server install directories that exist, as real paths.
func (w *vscodeWatch) serverDirs() []string {
	var dirs []string
	for _, root := range w.roots() {
		if real, err := filepath.EvalSymlinks(root.dir); err == nil {
			dirs = append(dirs, real)
		}
	}
	return dirs
}

// loopbackDial reports whether a direct-tcpip target is this host, where
// VS Code's server listens.
func loopbackDial(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// postAttach runs the merged postAttachCommand once for this connection.
func (w *vscodeWatch) postAttach(ctx context.Context) {
	cfg := w.configOrReport(ctx)
	if cfg == nil || len(cfg.PostAttach) == 0 {
		return
	}
	dir := cfg.WorkspaceFolder
	if info, err := os.Stat(dir); dir == "" || err != nil || !info.IsDir() {
		dir = w.c.opts.User.Home
	}
	w.c.rep.printf("==> postAttachCommand (VS Code attached)")
	for _, raw := range cfg.PostAttach {
		cmds, err := lifecycleCommands(raw)
		if err != nil {
			w.c.rep.printf("warning: postAttachCommand: %v", err)
			continue
		}
		var wg sync.WaitGroup
		failed := make([]error, len(cmds))
		for i, lc := range cmds {
			wg.Add(1)
			go func(i int, lc lifecycleCommand) {
				defer wg.Done()
				failed[i] = w.runLifecycle(ctx, dir, lc)
			}(i, lc)
		}
		wg.Wait()
		for i, err := range failed {
			if err != nil {
				name := ""
				if cmds[i].name != "" {
					name = " " + cmds[i].name
				}
				w.c.rep.printf("warning: postAttachCommand%s: %v", name, err)
				return // the Dev Containers extension stops at a failing command too
			}
		}
	}
	w.c.rep.printf("==> postAttachCommand: done")
}

func (w *vscodeWatch) runLifecycle(ctx context.Context, dir string, lc lifecycleCommand) error {
	cmd := exec.CommandContext(ctx, lc.argv[0], lc.argv[1:]...)
	cmd.Dir = dir
	cmd.Env = w.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	prefix := ""
	if lc.name != "" {
		prefix = lc.name + ": "
	}
	out := w.c.rep.writer(prefix)
	defer out.Flush()
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// lifecycleCommand is one command of a lifecycle hook.
type lifecycleCommand struct {
	name string // its key, for an object of named commands
	argv []string
}

// lifecycleCommands reads one metadata entry's lifecycle hook: a string
// (run by /bin/sh -c), an array (run as is), or an object of named
// commands of either kind (run in parallel).
func lifecycleCommands(raw json.RawMessage) ([]lifecycleCommand, error) {
	one := func(name string, raw json.RawMessage) (lifecycleCommand, bool, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if strings.TrimSpace(s) == "" {
				return lifecycleCommand{}, false, nil
			}
			return lifecycleCommand{name: name, argv: []string{"/bin/sh", "-c", s}}, true, nil
		}
		var argv []string
		if err := json.Unmarshal(raw, &argv); err == nil {
			if len(argv) == 0 {
				return lifecycleCommand{}, false, nil
			}
			return lifecycleCommand{name: name, argv: argv}, true, nil
		}
		return lifecycleCommand{}, false, fmt.Errorf("%s is neither a command line nor an argv", string(raw))
	}
	if lc, ok, err := one("", raw); err == nil {
		if !ok {
			return nil, nil
		}
		return []lifecycleCommand{lc}, nil
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, fmt.Errorf("%s is not a lifecycle command", string(raw))
	}
	names := make([]string, 0, len(named))
	for name := range named {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []lifecycleCommand
	for _, name := range names {
		lc, ok, err := one(name, named[name])
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, lc)
		}
	}
	return out, nil
}

func loadCustomizationState(path string) customizationState {
	var st customizationState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveCustomizationState(path string, st customizationState) {
	st.V = 1
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, append(data, '\n'), 0o600)
}

// lockFile takes an exclusive lock on path (created 0600), waiting for
// another connection's work until ctx ends.
func lockFile(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// writeFileAtomic replaces path with data (temp file + rename), with mode.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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

// tildePath writes path under home as ~/...
func tildePath(path, home string) string {
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return path
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
