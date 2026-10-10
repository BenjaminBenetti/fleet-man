package execas

// Class says whom a command runs as inside the pod, with which environment and
// in which directory. Each is a way fleet already reaches into a docker
// instance.
type Class string

const (
	// ClassExec is `devcontainer exec`: the remoteUser (else containerUser,
	// else root), the user's probed environment (userEnvProbe), SSH_AUTH_SOCK
	// and the config's remoteEnv, in the workspace folder. ExecCommand,
	// attaches and one-shots use it.
	ClassExec Class = "exec"
	// ClassScript is `docker exec -u <user>` with the same user as ClassExec,
	// so both reach one tmux server, but the bare container environment and
	// working directory. RunScript, the session polls and the mic and output
	// pipes use it.
	ClassScript Class = "script"
	// ClassRoot is `docker exec -u root`: system operations, such as installing
	// the audio stack.
	ClassRoot Class = "root"
	// ClassExplicit is a literal `docker exec [-u user] [-w dir] [-e k=v]`, as
	// the devcontainer CLI issues it for lifecycle hooks through the pod-mode
	// docker shim: the request names what it wants, and what it leaves out
	// defaults to the container's own (containerUser, its working directory).
	ClassExplicit Class = "explicit"
)

// Classes lists every class.
func Classes() []Class {
	return []Class{ClassExec, ClassScript, ClassRoot, ClassExplicit}
}

// Valid reports whether c is one of the classes.
func (c Class) Valid() bool {
	switch c {
	case ClassExec, ClassScript, ClassRoot, ClassExplicit:
		return true
	}
	return false
}
