package buildkit

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
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

// ConfigureInstanceBuildx points an instance's docker buildx at the fleet's
// shared buildkit server. It first probes for BOTH docker and the buildx plugin
// inside the instance; if either is missing it returns nil WITHOUT touching the
// instance — the documented "do nothing, no error" behaviour for images that
// lack docker tooling. When both are present it (re)creates a "remote" builder
// bound to the bind-mounted socket and selects it as the default.
//
// The configure step is idempotent (remove-then-create), so it is safe to call
// on every create, clone, and start. Callers should treat a returned error as
// non-fatal (warn and continue) — a build-cache wiring failure must not block
// an otherwise-usable instance.
func ConfigureInstanceBuildx(b instanceExecer, workspaceDir string) error {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", probeScript()}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe docker/buildx: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !buildxPresent(string(out)) {
		// docker or buildx absent — silently skip per the feature contract.
		return nil
	}

	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", configureScript()}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("configure buildx: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
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

// configureScript (re)creates the shared remote builder and selects it as the
// default. The remote driver speaks gRPC straight to the buildkitd unix socket
// at containerSocketPath (no in-instance daemon involved); the endpoint is the
// documented positional ENDPOINT argument to `buildx create` (not a
// --driver-opt). `use` is a separate command so the endpoint is never adjacent
// to a boolean flag. Remove-then-create keeps it idempotent across re-runs
// (clone inherits a prior builder; restart re-runs config); the `|| true` on rm
// tolerates a first run where the builder does not exist yet. The `&&` ensures a
// failed create propagates a non-zero exit (so the caller can warn) rather than
// being masked by a successful `use`.
func configureScript() string {
	return fmt.Sprintf(
		`docker buildx rm %s >/dev/null 2>&1 || true; docker buildx create --name %s --driver remote unix://%s && docker buildx use %s`,
		builderName, builderName, containerSocketPath, builderName,
	)
}

// buildxPresent reports whether the probe output indicates docker+buildx are
// available. It scans for the PRESENT marker as a substring so leading warnings
// on the combined stream don't mask a positive result.
func buildxPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}
