package buildkit

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// instanceExecer is the slice of backend.Backend this package needs to run
// commands inside an instance. Narrowing the dependency keeps the package
// testable with a tiny fake instead of the full Backend surface.
type instanceExecer interface {
	ExecCommand(workspaceDir string, command []string) *backend.Cmd
}

// probeMarkerPresent / probeMarkerAbsent are the single-token stdout signals the
// probe script emits so the outcome is unambiguous regardless of locale or
// extra warnings on stderr.
const (
	probeMarkerPresent = "PRESENT"
	probeMarkerAbsent  = "ABSENT"
)

// SocketEndpoint is the shared server's address for an instance on the
// daemon host's docker: buildkitd's socket in the bind-mounted directory
// (InstanceMount).
const SocketEndpoint = "unix://" + containerSocketPath

// CredentialDir is where an instance keeps the client credential a server
// that authenticates instances takes (ca.pem, cert.pem, key.pem):
// ~/.fleet/buildkit of the user fleet runs commands as, which the
// instance's shell expands.
const CredentialDir = "$HOME/.fleet/buildkit"

// Builder is the buildkitd an instance's shared builder talks to.
type Builder struct {
	// Endpoint is its address as the instance reaches it (SocketEndpoint
	// on the daemon host's docker, tcp://<host>:<port> on a cluster).
	Endpoint string
	// ServerName, when set, makes the builder speak mutual TLS: it presents
	// the client certificate InstallCredential put in CredentialDir and
	// checks the server's certificate is for ServerName.
	ServerName string
}

// SocketBuilder is the shared server on the daemon host's docker.
func SocketBuilder() Builder { return Builder{Endpoint: SocketEndpoint} }

// What a Builder may hold: the script quotes both, but buildx reads its
// driver options as comma-separated pairs, so a server name is a DNS name
// and nothing else.
var (
	endpointPattern   = regexp.MustCompile(`^(unix|tcp)://[A-Za-z0-9._/:\[\]-]+$`)
	serverNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

func (b Builder) validate() error {
	if !endpointPattern.MatchString(b.Endpoint) {
		return fmt.Errorf("buildkit endpoint %q is not a unix:// or tcp:// address", b.Endpoint)
	}
	if b.ServerName != "" && !serverNamePattern.MatchString(b.ServerName) {
		return fmt.Errorf("buildkit server name %q is not a DNS name", b.ServerName)
	}
	return nil
}

// ConfigureInstanceBuildx points an instance's docker buildx at the fleet's
// shared buildkit server (builder). It first probes for BOTH docker and the
// buildx plugin inside the instance; if either is missing it returns (false,
// nil) WITHOUT touching the instance — the documented "do nothing, no error"
// behaviour for images that lack docker tooling. When both are present it
// (re)creates a "remote" builder for builder and selects it as the default.
//
// The returned bool reports whether buildx was actually wired (both tools
// present): a probe error yields (false, err); the silent-skip branch yields
// (false, nil); a configure-script failure yields (true, err); success yields
// (true, nil).
//
// The configure step is idempotent (remove-then-create), so it is safe to call
// on every create, clone and rebuild. Callers should treat a returned error as
// non-fatal (warn and continue) — a build-cache wiring failure must not block
// an otherwise-usable instance.
func ConfigureInstanceBuildx(b instanceExecer, workspaceDir string, builder Builder) (configured bool, err error) {
	return configureBuildx(b, workspaceDir, builder, false)
}

// EnsureInstanceBuildx is ConfigureInstanceBuildx for a start: an instance
// that has the shared builder keeps it as it is (and keeps whichever builder
// its user made the default); one without it gets it, as the default.
func EnsureInstanceBuildx(b instanceExecer, workspaceDir string, builder Builder) (configured bool, err error) {
	return configureBuildx(b, workspaceDir, builder, true)
}

func configureBuildx(b instanceExecer, workspaceDir string, builder Builder, keep bool) (bool, error) {
	script, err := configureScript(builder, keep)
	if err != nil {
		return false, err
	}
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", probeScript()}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("probe docker/buildx: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !buildxPresent(string(out)) {
		// docker or buildx absent — silently skip per the feature contract.
		return false, nil
	}

	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput()
	if err != nil {
		return true, fmt.Errorf("configure buildx: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// probeScript reports whether docker AND the buildx plugin are usable inside the
// instance. It always exits 0 and prints exactly one marker so an exec failure
// (a non-zero exit / unreachable container) is distinguishable from a clean
// "not present" answer. `docker buildx version` needs no daemon — it only
// verifies the CLI plugin is installed.
func probeScript() string {
	return fmt.Sprintf(
		`if command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then echo %s; else echo %s; fi`,
		probeMarkerPresent, probeMarkerAbsent,
	)
}

// configureScript (re)creates the shared remote builder for builder and
// selects it as the default. The remote driver speaks gRPC straight to
// buildkitd (no in-instance daemon involved); the endpoint is the documented
// positional ENDPOINT argument to `buildx create`. A builder that speaks TLS
// names its credential files by absolute path (buildx refuses relative ones),
// which the shell expands from CredentialDir; buildx reads them on every
// connection, so a renewed credential needs no new builder. `use` is a
// separate command so the endpoint is never adjacent to a boolean flag.
// Remove-then-create keeps it idempotent across re-runs (clone inherits a
// prior builder; a rebuild re-runs config); the `|| true` on rm tolerates a
// first run where the builder does not exist yet. The `&&` ensures a failed
// create propagates a non-zero exit (so the caller can warn) rather than
// being masked by a successful `use`. With keep, an existing builder of the
// name is left as it is and nothing else runs.
func configureScript(builder Builder, keep bool) (string, error) {
	if err := builder.validate(); err != nil {
		return "", err
	}
	var s strings.Builder
	if keep {
		fmt.Fprintf(&s, "if docker buildx inspect %s >/dev/null 2>&1; then exit 0; fi; ", builderName)
	} else {
		fmt.Fprintf(&s, "docker buildx rm %s >/dev/null 2>&1 || true; ", builderName)
	}
	create := fmt.Sprintf("docker buildx create --name %s --driver remote", builderName)
	if builder.ServerName != "" {
		fmt.Fprintf(&s, `d="%s"; n=%s; `, CredentialDir, shellquote.Single(builder.ServerName))
		create += ` --driver-opt "cacert=$d/ca.pem,cert=$d/cert.pem,key=$d/key.pem,servername=$n"`
	}
	fmt.Fprintf(&s, "%s %s && docker buildx use %s", create, shellquote.Single(builder.Endpoint), builderName)
	return s.String(), nil
}

// buildxPresent reports whether the probe output indicates docker+buildx are
// available. It scans for the PRESENT marker as a substring so leading warnings
// on the combined stream don't mask a positive result.
func buildxPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}

// credentialInstaller is the slice of backend.Backend InstallCredential
// needs: quiet commands, and the backend's file transfer (CopyFile), so no
// byte of the credential is in a command's argv or in fleet.log.
type credentialInstaller interface {
	ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd
	CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error
}

// The credential's files in CredentialDir, in the order they are moved into
// place, with their modes: the CA and the client certificate
// world-readable, the key the user's alone.
var credentialFiles = []struct {
	name string
	mode int
	data func(fleet.ServiceCredential) []byte
}{
	{"ca.pem", 0o644, func(c fleet.ServiceCredential) []byte { return c.CA }},
	{"key.pem", 0o600, func(c fleet.ServiceCredential) []byte { return c.Key }},
	{"cert.pem", 0o644, func(c fleet.ServiceCredential) []byte { return c.Cert }},
}

// credentialStage prefixes a credential file copied in beside its place,
// before the three are moved in.
const credentialStage = ".fleet-new-"

// credentialDirMarker starts the line credentialDirScript prints the
// folder's absolute path on.
const credentialDirMarker = "FLEET_CREDENTIAL_DIR="

// credentialDirScript makes CredentialDir the user's alone and prints its
// absolute path (CopyFile takes no $HOME).
var credentialDirScript = fmt.Sprintf(`set -e; umask 077; d="%s"; mkdir -p "$d"; chmod 700 "$d"; printf '%s%%s\n' "$d"`, CredentialDir, credentialDirMarker)

// InstallCredential puts cred in the instance's CredentialDir, where a
// builder made for a server that authenticates instances reads it: the CA
// and the client certificate world-readable, the key the user's alone, the
// directory too. The files go through the backend's CopyFile, never in a
// command (an argv is in the process list, and a logged command in
// fleet.log), and all three are copied in beside their places before any of
// them is moved in, so a failed copy leaves the last credential whole.
// Unlike the builder it does not need docker in the instance: a docker
// installed later finds its credential there.
func InstallCredential(b credentialInstaller, workspaceDir string, cred fleet.ServiceCredential) error {
	if len(cred.CA) == 0 || len(cred.Cert) == 0 || len(cred.Key) == 0 {
		return fmt.Errorf("an incomplete buildkit credential (CA %d, certificate %d, key %d bytes)", len(cred.CA), len(cred.Cert), len(cred.Key))
	}
	out, err := b.ExecCommandQuiet(workspaceDir, []string{"sh", "-c", credentialDirScript}).CombinedOutputWithTimeout(backend.CopyTimeout)
	if err != nil {
		return fmt.Errorf("make the buildkit credential folder: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	dir, err := credentialDir(string(out))
	if err != nil {
		return err
	}
	for _, f := range credentialFiles {
		if err := b.CopyFile(workspaceDir, bytes.NewReader(f.data(cred)), path.Join(dir, credentialStage+f.name), f.mode); err != nil {
			if out, rerr := b.ExecCommandQuiet(workspaceDir, []string{"sh", "-c", credentialMoveScript(dir, false)}).CombinedOutput(); rerr != nil {
				err = fmt.Errorf("%w; removing the copies: %v (%s)", err, rerr, strings.TrimSpace(string(out)))
			}
			return fmt.Errorf("copy the buildkit credential's %s into the instance: %w", f.name, err)
		}
	}
	if out, err := b.ExecCommandQuiet(workspaceDir, []string{"sh", "-c", credentialMoveScript(dir, true)}).CombinedOutputWithTimeout(backend.CopyTimeout); err != nil {
		return fmt.Errorf("install the buildkit credential: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// credentialDir is the absolute folder credentialDirScript printed, cleaned
// (a HOME of "/" or with a trailing slash prints a doubled one).
func credentialDir(out string) (string, error) {
	for _, l := range strings.Split(out, "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(l), credentialDirMarker); ok && path.IsAbs(d) {
			return path.Clean(d), nil
		}
	}
	return "", fmt.Errorf("the instance named no absolute buildkit credential folder (%s)", strings.TrimSpace(out))
}

// credentialMoveScript moves the copies InstallCredential made in dir into
// place (install), or removes them (a copy failed).
func credentialMoveScript(dir string, install bool) string {
	var s strings.Builder
	fmt.Fprintf(&s, "d=%s; ", shellquote.Single(dir))
	if !install {
		s.WriteString("rm -f")
		for _, f := range credentialFiles {
			fmt.Fprintf(&s, ` "$d/%s%s"`, credentialStage, f.name)
		}
		return s.String()
	}
	s.WriteString("set -e")
	for _, f := range credentialFiles {
		fmt.Fprintf(&s, `; mv -f "$d/%s%s" "$d/%s"`, credentialStage, f.name, f.name)
	}
	return s.String()
}

// RemoveCredential deletes the client certificate and key in the instance's
// CredentialDir (a clone's copy of its source's, when the clone could not
// get its own). Best-effort: there may be none.
func RemoveCredential(b instanceExecer, workspaceDir string) error {
	script := fmt.Sprintf(`d="%s"; rm -f "$d/cert.pem" "$d/key.pem"`, CredentialDir)
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput(); err != nil {
		return fmt.Errorf("remove the buildkit credential: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
