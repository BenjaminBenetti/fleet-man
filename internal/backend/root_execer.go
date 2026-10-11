package backend

// RootExecer is an optional capability for a backend that can run a command
// as root inside an instance itself, whether or not the instance's session
// user may (no sudo needed): a Kubernetes pod's agent runs a root class.
// Provisioning runs a script's privileged steps through it (a package
// install, a write under /etc) and the rest as the session user, instead of
// running the whole script as the session user with sudo. Backends that reach
// root only through the image's own sudo do not implement it.
type RootExecer interface {
	// RootCapable reports whether the bound instance has root at all. A pod
	// on a cluster that runs every container as one non-root uid has none:
	// its root class is that uid, and a privileged step cannot run there.
	// It reads what the backend keeps of the instance on the daemon host,
	// never the instance itself.
	RootCapable() bool
	// RootExecCommand is ExecCommand run as root (uid 0, root's
	// environment) instead of as the session user.
	RootExecCommand(workspaceDir string, command []string) *Cmd
}
