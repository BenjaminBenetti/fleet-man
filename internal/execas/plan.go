package execas

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
)

// Host is what execas knows about the pod it runs in. NewHost (linux) builds
// the one for this process; tests build their own.
type Host struct {
	// Spec is the pod's exec spec.
	Spec Spec
	// Root is the directory holding the container's etc/passwd and etc/group:
	// "/" in a pod ("" means "/"). Working directories and executables are
	// always looked up on the real filesystem.
	Root string
	// ProcessEnv is the environment this process got from the container
	// runtime (the agent's or exec-as's os.Environ()): the container's
	// environment, plus the HOME the runtime set for this process.
	ProcessEnv []string
	// Self is this process's own uid, gid and groups.
	Self Creds
	// SingleUID is capability tier C: this process cannot switch users, so
	// every class runs as Self, the pod uid.
	SingleUID bool
	// Prober reads the exec class's user environment; nil probes nothing.
	Prober EnvProber
	// Bundles, when set, lists the program bundles fleet staged into the pod
	// (the static tmux of an image that has none). Every command, whatever
	// its class, finds their programs last on its PATH (the image's own come
	// first) and their terminfo after the system's. It is asked on every
	// Plan, so a bundle installed after this Host was made counts.
	Bundles func() []Bundle
	// ProbeDeps are files besides the startup files whose state the probed
	// environment depends on (DefaultProbeDeps in a pod).
	ProbeDeps []string
	// AgentSocket reports whether the exec class's SSH_AUTH_SOCK (the spec's
	// SSHAuthSock) is there to export; nil looks for a socket at the path.
	// The pod agent binds it only while the daemon can serve the pod an
	// agent, so a command started without one gets no SSH_AUTH_SOCK that
	// could only fail, as docker passes none then.
	AgentSocket func(path string) bool
}

// Bundle is one program bundle in a pod: Bin holds its programs, Terminfo
// ("" for none) the terminal descriptions they were built to read.
type Bundle struct {
	Bin      string
	Terminfo string
}

// NewHost is the Host this process runs in, inside a pod: the container's
// /etc, the environment and credentials it was started with, single-uid mode
// when it cannot switch users, and a Prober caching under cacheDir ("" keeps
// the cache in memory only).
func NewHost(spec Spec, cacheDir string) *Host {
	return &Host{
		Spec:       spec,
		Root:       "/",
		ProcessEnv: os.Environ(),
		Self:       CurrentCreds(),
		SingleUID:  !CanSwitchUser(),
		Prober:     NewProber(cacheDir),
		ProbeDeps:  DefaultProbeDeps,
	}
}

// Plan is a request resolved against the pod: everything Start or Exec needs.
type Plan struct {
	Class    Class
	Identity Identity
	// Switch is whether the command takes Identity's credentials (setgroups,
	// setgid, setuid). It is false in single-uid mode, where the command keeps
	// this process's own.
	Switch bool
	// Path is the executable: Argv[0], looked up on the command's own PATH as
	// its user would find it. A relative Path is relative to Dir.
	Path string
	Argv []string
	Env  []string
	Dir  string
	TTY  bool
	Rows uint16
	Cols uint16
	// TTYGroup is the group a PTY is chowned to: the container's tty group,
	// else the user's primary group.
	TTYGroup uint32
	// Cgroup, when set, is the cgroup v2 directory the command starts in (a
	// privileged pod's user cgroup, which a clone freezes); "" leaves it in
	// the starter's own. The caller sets it; Plan never does.
	Cgroup string
}

// Plan resolves req: whom it runs as, its environment, its working directory
// and its executable. A failure is an *ExitError with the code `docker exec`
// gives for it; ctx ending during the exec class's probe is one (ExitFailed).
func (h *Host) Plan(ctx context.Context, req Request) (*Plan, error) {
	if err := req.Validate(); err != nil {
		return nil, failed(err)
	}
	if err := h.Spec.Validate(); err != nil {
		return nil, failed(fmt.Errorf("exec spec: %w", err))
	}
	db, err := LoadDB(h.root())
	if err != nil {
		return nil, failed(err)
	}
	id, err := h.identity(db, req)
	if err != nil {
		return nil, cannotRun(err)
	}

	// docker exec's environment: the daemon's defaults (PATH; TERM with a
	// TTY), under the container's environment, under -e; runc then sets HOME
	// when nothing did. Setting a default last is the same as putting it
	// first and letting every later layer override it.
	cenv := containerEnv(h.ProcessEnv, db.identityOf(h.Self).Home)
	env := cenv.clone()
	env.setDefault("PATH", DefaultPath)
	if req.Class == ClassExec {
		if err := h.addUserEnv(ctx, env, cenv, id); err != nil {
			return nil, failed(err)
		}
	}
	env.merge(req.Env)
	if h.Bundles != nil {
		addBundles(env, h.Bundles())
	}
	env.setDefault("HOME", id.Home)
	h.addHomeEnv(env, id)
	if req.TTY {
		env.setDefault("TERM", "xterm")
	}

	dir := req.Cwd
	if dir == "" {
		dir = h.defaultDir(req.Class, env, id)
	}
	pathEnv, _ := env.get("PATH")
	exe, err := lookPath(req.Argv[0], pathEnv, dir, id)
	if err != nil {
		return nil, err
	}

	ttyGroup := id.GID
	if g, ok := db.GroupByName("tty"); ok {
		ttyGroup = uint32(g.GID)
	}
	return &Plan{
		Class:    req.Class,
		Identity: id,
		Switch:   !h.SingleUID,
		Path:     exe,
		Argv:     slices.Clone(req.Argv),
		Env:      env.entries(),
		Dir:      dir,
		TTY:      req.TTY,
		Rows:     req.Rows,
		Cols:     req.Cols,
		TTYGroup: ttyGroup,
	}, nil
}

// Resolve is whom a request of class, naming user (docker's user[:group]; ""
// for the class's own), runs as: the identity Plan resolves, for work that
// starts no command, such as the pod agent's file writes and reads. In
// single-uid mode it is this process. A failure is an *ExitError, as Plan's.
func (h *Host) Resolve(class Class, user string) (Identity, error) {
	req := Request{Class: class, User: user, Argv: []string{"true"}}
	if err := req.Validate(); err != nil {
		return Identity{}, failed(err)
	}
	db, err := LoadDB(h.root())
	if err != nil {
		return Identity{}, failed(err)
	}
	id, err := h.identity(db, req)
	if err != nil {
		return Identity{}, cannotRun(err)
	}
	return id, nil
}

func (h *Host) root() string {
	if h.Root == "" {
		return "/"
	}
	return h.Root
}

// identity is whom req runs as. In single-uid mode that is this process,
// whatever the class asks for. Otherwise it is the request's user, else the
// class's, with the groups the runtime gave the whole container added.
func (h *Host) identity(db *DB, req Request) (Identity, error) {
	if h.SingleUID {
		return db.identityOf(h.Self), nil
	}
	user := req.User
	if user == "" || req.Class == ClassRoot {
		user = h.defaultUser(req.Class)
	}
	id, err := db.Lookup(user)
	if err != nil {
		return Identity{}, err
	}
	for _, g := range db.runtimeGroups(h.Self) {
		if !slices.Contains(id.Groups, g) {
			id.Groups = append(id.Groups, g)
		}
	}
	return id, nil
}

// defaultUser is whom a class runs as when the request names nobody. The exec
// and script classes share one user, as the devcontainer CLI picks it
// (remoteUser, else the container's user); explicit is a bare `docker exec`
// (the container's user); root is uid 0. An unset container user is uid 0, as
// docker runs it even in an image with no passwd entry for root.
func (h *Host) defaultUser(class Class) string {
	switch class {
	case ClassExec, ClassScript:
		if h.Spec.RemoteUser != "" {
			return h.Spec.RemoteUser
		}
	case ClassRoot:
		return "0"
	}
	if h.Spec.ContainerUser != "" {
		return h.Spec.ContainerUser
	}
	return "0"
}

// defaultDir is where a class starts when the request names no directory: the
// exec class in the workspace folder, else the user's home (the CLI's rule);
// the others in the container's working directory, as `docker exec` does.
func (h *Host) defaultDir(class Class, env *envList, id Identity) string {
	if class != ClassExec {
		if h.Spec.WorkingDir != "" {
			return h.Spec.WorkingDir
		}
		return "/"
	}
	if h.Spec.WorkspaceFolder != "" {
		return h.Spec.WorkspaceFolder
	}
	if home, _ := env.get("HOME"); path.IsAbs(home) {
		return home
	}
	return id.Home
}

// addHomeEnv sets the spec's HomeEnv variables env leaves unset, each the
// home's folder its path names (the command's HOME, else its user's).
func (h *Host) addHomeEnv(env *envList, id Identity) {
	if len(h.Spec.HomeEnv) == 0 {
		return
	}
	home, _ := env.get("HOME")
	if !path.IsAbs(home) {
		home = id.Home
	}
	if !path.IsAbs(home) {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(h.Spec.HomeEnv)) {
		env.setDefault(name, path.Join(home, h.Spec.HomeEnv[name]))
	}
}

// addUserEnv layers what `devcontainer exec` passes as -e over env: the
// probed user environment, then fleet's --remote-env (SSH_AUTH_SOCK), then the
// config's remoteEnv, in the CLI's order. cenv is the container environment the
// remoteEnv's ${containerEnv:...} references resolve against.
func (h *Host) addUserEnv(ctx context.Context, env, cenv *envList, id Identity) error {
	if mode := h.Spec.UserEnvProbe.Effective(); mode != ProbeNone && h.Prober != nil {
		start := env.clone()
		start.setDefault("HOME", id.Home)
		shell, _ := cenv.get("SHELL")
		if shell == "" {
			shell = id.Shell
		}
		if shell == "" {
			shell = "/bin/sh"
		}
		env.merge(h.Prober.ProbeEnv(ctx, ProbeTarget{
			Mode:     mode,
			Shell:    shell,
			Identity: id,
			Switch:   !h.SingleUID,
			Env:      start.entries(),
			Dir:      h.probeDir(),
			Deps:     h.ProbeDeps,
		}))
		// A probe cut short by ctx answered nothing: the request was
		// cancelled, so it gets no plan rather than one without the user's
		// environment.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("probe the user environment: %w", err)
		}
	}
	if sock := h.Spec.SSHAuthSock; sock != "" && h.agentSocket(sock) {
		env.set("SSH_AUTH_SOCK", sock)
	}
	for _, name := range slices.Sorted(maps.Keys(h.Spec.RemoteEnv)) {
		value, err := substituteContainerEnv(h.Spec.RemoteEnv[name], cenv)
		if err != nil {
			return fmt.Errorf("remoteEnv %s: %w", name, err)
		}
		env.set(name, value)
	}
	return nil
}

// agentSocket reports whether the agent socket at path is there to export.
func (h *Host) agentSocket(path string) bool {
	if h.AgentSocket != nil {
		return h.AgentSocket(path)
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// probeDir is where the probe shell starts: the container's working directory,
// as the CLI's probe (a bare `docker exec`) does, or / when that is gone.
func (h *Host) probeDir() string {
	if dir := h.Spec.WorkingDir; dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return "/"
}
