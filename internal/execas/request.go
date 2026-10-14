package execas

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Request is one command to run in the pod. The daemon sends it to the pod
// agent, or frames it on the stdin of `fleet exec-as` (WriteRequest), never in
// argv: the API server logs exec argv, and argv carries scripts and variables.
// Unknown fields are ignored, so an older in-pod binary takes a newer daemon's
// request.
type Request struct {
	// Class is whom the command runs as, and how.
	Class Class `json:"class"`
	// Argv is the command line; it is not run through a shell. A bare argv[0]
	// is looked up on the command's own PATH.
	Argv []string `json:"argv"`
	// User overrides the class's user, in docker's user[:group] form: the
	// exec, script and explicit classes take it (explicit is how the docker
	// shim passes `-u`). The root class takes only "" or root.
	User string `json:"user,omitempty"`
	// Cwd overrides the class's working directory; it must be absolute.
	Cwd string `json:"cwd,omitempty"`
	// Env is added to the command's environment last, so it wins (docker's
	// -e).
	Env map[string]string `json:"env,omitempty"`
	// TTY runs the command on a pseudo-terminal of Rows x Cols (0 leaves the
	// size to the terminal's default).
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Validate checks the request is complete and well formed.
func (r Request) Validate() error {
	if !r.Class.Valid() {
		return fmt.Errorf("unknown exec class %q", r.Class)
	}
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("no command to run")
	}
	for _, arg := range r.Argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return errors.New("command line contains a NUL byte")
		}
	}
	if strings.IndexByte(r.User, 0) >= 0 {
		return errors.New("user contains a NUL byte")
	}
	if r.Class == ClassRoot && r.User != "" && r.User != "root" && r.User != "0" {
		return fmt.Errorf("the root class runs as root, not %q", r.User)
	}
	if r.Cwd != "" && (!path.IsAbs(r.Cwd) || strings.IndexByte(r.Cwd, 0) >= 0) {
		return fmt.Errorf("working directory %q is not an absolute path", r.Cwd)
	}
	for name, value := range r.Env {
		if err := checkEnvVar(name, value); err != nil {
			return err
		}
	}
	if !r.TTY && (r.Rows != 0 || r.Cols != 0) {
		return errors.New("a terminal size needs tty")
	}
	return nil
}
