package podspec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// Secrets are the contents of an instance's two Secrets. The pod spec and
// its annotations never hold these values (Kubernetes gives Secrets their
// own access control; pod and PVC reads are wider).
type Secrets struct {
	// Spec is <sts>-spec, mounted read-only at /opt/fleet/spec: the files
	// pod-init, the pod agent and the pod-mode shim read (pid1.json,
	// exec.json, inspect.json, merged.json, capture.json), by name.
	Spec map[string][]byte
	// Env is <sts>-env: the container environment's values, by the key each
	// PodSpec.Env entry names (secretKeyRef). Nothing reaches the kubelet's
	// $(VAR) expansion: the values arrive through valueFrom.
	Env map[string][]byte
}

// The files of the spec Secret besides pid1.json.
const (
	// ExecFile is execas.Spec, what `devcontainer exec` knows about the
	// instance (execas.DefaultSpecPath).
	ExecFile = "exec.json"
	// InspectFile is the kubeshim.Container the pod-mode shim answers
	// `docker inspect` with; the pod's identity (ContainerID, Name, Created,
	// StartedAt) is filled in when the shim runs.
	InspectFile = "inspect.json"
	// MergedFile is the merged devcontainer configuration.
	MergedFile = "merged.json"
	// CaptureFile is the captured docker run with every environment value
	// redacted (kubeshim.Capture.Redacted).
	CaptureFile = "capture.json"
)

// maxSecretData is how much a Secret holds: the API server's 1 MiB object
// limit less room for the object's metadata.
const maxSecretData = 1<<20 - 16<<10

// secretKeyRE is a valid Secret data key.
var secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// envKey is the Secret key a variable's value is kept under: its name when
// that is a valid key, else a digest of it (an environment variable name
// may hold characters a key cannot).
func envKey(name string) string {
	if len(name) <= 253 && secretKeyRE.MatchString(name) && !strings.HasPrefix(name, "x-") {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "x-" + hex.EncodeToString(sum[:8])
}

// finishEnv references each variable the run sets from the environment
// Secret. A bare -e NAME whose variable the daemon host did not have
// removes NAME from the image's environment on docker; a pod cannot remove
// an image variable, so it keeps the image's value, with a warning.
func (t *translation) finishEnv() {
	imageEnv := map[string]bool{}
	for _, e := range t.in.Image.Config.Env {
		name, _, _ := strings.Cut(e, "=")
		imageEnv[name] = true
	}
	for _, e := range t.in.Capture.Env {
		if e.Unset {
			if imageEnv[e.Name] {
				t.warn("-e %s: %s is not set on the daemon host, so docker would remove the image's %s; the pod keeps the image's value", e.Name, e.Name, e.Name)
			}
			continue
		}
		if e.Name == "" || strings.ContainsAny(e.Name, "=\x00") {
			t.refuse(refuseOption("env", fmt.Sprintf("invalid environment variable name %q", e.Name)))
			continue
		}
		if strings.IndexByte(e.Value, 0) >= 0 {
			t.refuse(refuseOption("env", fmt.Sprintf("the value of %s has a NUL byte", e.Name)))
			continue
		}
		t.spec.Env = append(t.spec.Env, EnvRef{Name: e.Name, Key: envKey(e.Name)})
	}
}

// secrets renders the two Secrets.
func (t *translation) secrets() (Secrets, error) {
	s := Secrets{Spec: map[string][]byte{}, Env: map[string][]byte{}}
	values := map[string]string{}
	for _, e := range t.in.Capture.Env {
		if !e.Unset {
			values[e.Name] = e.Value
		}
	}
	for _, ref := range t.spec.Env {
		if _, dup := s.Env[ref.Key]; dup {
			return Secrets{}, fmt.Errorf("podspec: two environment variables share the Secret key %s", ref.Key)
		}
		s.Env[ref.Key] = []byte(values[ref.Name])
	}

	files := map[string]any{
		PID1File:    t.spec.PID1,
		ExecFile:    t.exec,
		InspectFile: t.container,
		CaptureFile: t.in.Capture.Redacted(),
	}
	for _, name := range sortedKeys(files) {
		data, err := marshalFile(files[name])
		if err != nil {
			return Secrets{}, fmt.Errorf("podspec: rendering %s: %w", name, err)
		}
		s.Spec[name] = data
	}
	merged := []byte(strings.TrimSpace(string(t.in.Merged)))
	if len(merged) == 0 {
		merged = []byte("{}")
	}
	s.Spec[MergedFile] = merged

	for name, data := range map[string]map[string][]byte{"spec": s.Spec, "env": s.Env} {
		if n := secretSize(data); n > maxSecretData {
			return Secrets{}, &Refusal{Code: CodeSpecTooLarge, Reason: fmt.Sprintf("the instance's %s Secret would hold %d bytes; a Secret holds at most %d", name, n, maxSecretData)}
		}
	}
	return s, nil
}

// marshalFile writes a spec file as compact JSON, leaving <, > and & as
// they are (the redaction placeholder reads as written).
func marshalFile(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func secretSize(data map[string][]byte) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

// finishExec writes exec.json: what `devcontainer exec` knows about the
// instance, from the merged configuration and the container docker would
// have run. A remoteEnv variable set to null (which unsets it for the CLI's
// execs) is left out, so it keeps the container's value.
func (t *translation) finishExec() {
	spec := execas.Spec{
		RemoteUser:      t.merged.RemoteUser,
		ContainerUser:   t.container.User,
		WorkspaceFolder: t.in.WorkspaceFolder,
		WorkingDir:      t.container.WorkingDir,
		UserEnvProbe:    execas.ProbeMode(t.merged.UserEnvProbe),
		SSHAuthSock:     t.in.SSHAuthSock,
	}
	var unset []string
	for _, name := range sortedKeys(t.merged.RemoteEnv) {
		v := t.merged.RemoteEnv[name]
		if v == nil {
			unset = append(unset, name)
			continue
		}
		if spec.RemoteEnv == nil {
			spec.RemoteEnv = map[string]string{}
		}
		spec.RemoteEnv[name] = *v
	}
	if len(unset) > 0 {
		t.warn("remoteEnv %s: null does not unset a variable in a pod's execs; each keeps the container's value", strings.Join(unset, ", "))
	}
	if err := spec.Validate(); err != nil {
		t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("the devcontainer's exec settings: %v", err)})
		return
	}
	t.exec = spec
}
