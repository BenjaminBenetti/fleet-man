package execas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// DefaultSpecPath is where a pod finds its exec spec: exec.json in the
// <sts>-spec Secret, mounted at /opt/fleet/spec.
const DefaultSpecPath = "/opt/fleet/spec/exec.json"

// maxSpecSize caps the exec spec read from disk.
const maxSpecSize = 4 << 20

// Spec is the pod's exec.json: what the devcontainer CLI knows about an
// instance when it runs `exec`, rendered by the daemon from the merged
// configuration. It is the schema exec.json is written in. Unknown fields are
// ignored, so a newer daemon's spec still works with an older in-pod binary.
type Spec struct {
	// RemoteUser is the merged config's remoteUser; "" when it has none.
	RemoteUser string `json:"remote_user,omitempty"`
	// ContainerUser is whom the container runs as (`docker run -u`: the merged
	// containerUser, else the image's USER), in docker's user[:group] form.
	// "" means root.
	ContainerUser string `json:"container_user,omitempty"`
	// WorkspaceFolder is the remote workspace folder, where the exec class
	// starts.
	WorkspaceFolder string `json:"workspace_folder,omitempty"`
	// WorkingDir is the container's working directory (`docker run -w`, else
	// the image's WORKDIR), where the other classes start. "" means /.
	WorkingDir string `json:"working_dir,omitempty"`
	// UserEnvProbe is the merged config's userEnvProbe; "" is the CLI's
	// default, loginInteractiveShell.
	UserEnvProbe ProbeMode `json:"user_env_probe,omitempty"`
	// RemoteEnv is the merged config's remoteEnv, every variable substituted
	// except ${containerEnv:...}: those resolve against the container
	// environment at exec time, as the CLI resolves them.
	RemoteEnv map[string]string `json:"remote_env,omitempty"`
	// SSHAuthSock is the agent socket the exec class exports as SSH_AUTH_SOCK,
	// what fleet passes as --remote-env on docker, while a socket is there
	// (Host.AgentSocket). "" exports none.
	SSHAuthSock string `json:"ssh_auth_sock,omitempty"`
	// HomeEnv are fleet's own variables naming a folder below the home of
	// whoever a command runs as (a slash-separated path relative to it):
	// every class gets each, the home joined with its path, unless its
	// environment already sets the variable (the container's, the user's,
	// remoteEnv or the request's). On a fleet share mounted from several
	// nodes, CODEX_SQLITE_HOME keeps Codex's SQLite state on the instance's
	// own disk this way.
	HomeEnv map[string]string `json:"home_env,omitempty"`
}

// LoadSpec reads and checks the exec spec at file.
func LoadSpec(file string) (Spec, error) {
	f, err := os.Open(file)
	if err != nil {
		return Spec{}, fmt.Errorf("read exec spec: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSpecSize+1))
	if err != nil {
		return Spec{}, fmt.Errorf("read exec spec %s: %w", file, err)
	}
	if len(data) > maxSpecSize {
		return Spec{}, fmt.Errorf("exec spec %s is larger than %d bytes", file, maxSpecSize)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse exec spec %s: %w", file, err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("exec spec %s: %w", file, err)
	}
	return spec, nil
}

// Validate checks the spec's fields are usable as written.
func (s Spec) Validate() error {
	if !s.UserEnvProbe.Valid() {
		return fmt.Errorf("unknown user_env_probe %q", s.UserEnvProbe)
	}
	for name, dir := range map[string]string{
		"workspace_folder": s.WorkspaceFolder,
		"working_dir":      s.WorkingDir,
		"ssh_auth_sock":    s.SSHAuthSock,
	} {
		if dir != "" && !path.IsAbs(dir) {
			return fmt.Errorf("%s %q is not an absolute path", name, dir)
		}
		if strings.IndexByte(dir, 0) >= 0 {
			return fmt.Errorf("%s contains a NUL byte", name)
		}
	}
	for name, value := range s.RemoteEnv {
		if err := checkEnvVar(name, value); err != nil {
			return fmt.Errorf("remote_env: %w", err)
		}
	}
	for name, rel := range s.HomeEnv {
		if err := checkEnvVar(name, rel); err != nil {
			return fmt.Errorf("home_env: %w", err)
		}
		if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("home_env %s: %q is not a clean path below the home", name, rel)
		}
	}
	return nil
}

// checkEnvVar reports whether name=value can go in a process environment.
func checkEnvVar(name, value string) error {
	if name == "" {
		return errors.New("empty variable name")
	}
	if strings.ContainsAny(name, "=\x00") {
		return fmt.Errorf("variable name %q contains '=' or NUL", name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("variable %s has a NUL byte in its value", name)
	}
	return nil
}
