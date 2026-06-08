package imagecache

import (
	"fmt"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// Polling schedule for ConfigureInstanceDockerPolling. Declared as vars so tests
// can shrink them. The first pollSyncAttempts probes run synchronously; the rest
// run in the background until pollTimeout has elapsed since the first probe.
var (
	pollInterval     = 15 * time.Second
	pollTimeout      = 5 * time.Minute
	pollSyncAttempts = 3
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

// ConfigureInstanceDocker points an instance's OWN docker daemon at the fleet's
// shared image cache by adding registry-mirrors + insecure-registries to
// /etc/docker/daemon.json and reloading dockerd. It first probes for a LOCAL
// dockerd (docker-in-docker); if there is none — docker-outside-of-docker or no
// docker — it returns nil WITHOUT touching the instance (the documented "do
// nothing, no error" behaviour). The daemon.json merge is itself best-effort and
// never clobbers an existing file it can't safely merge.
//
// Callers should treat a returned error as non-fatal (warn and continue).
//
// This is the single-shot variant (probe once, give up if absent). New callers
// generally want ConfigureInstanceDockerPolling, which tolerates a dind that has
// not finished starting yet.
func ConfigureInstanceDocker(b instanceExecer, workspaceDir, mirrorURL, hostPort string) error {
	_, err := tryConfigureOnce(b, workspaceDir, mirrorURL, hostPort)
	return err
}

// tryConfigureOnce runs ONE probe and, if an in-instance dockerd is present,
// writes the mirror config. present reports whether a dockerd was found (configure
// was attempted; err is the configure error if any). When present is false the
// instance has no configurable dockerd yet (or at all) and err is any probe error
// — the caller decides whether to retry or treat it as the documented "do nothing,
// no error" skip.
func tryConfigureOnce(b instanceExecer, workspaceDir, mirrorURL, hostPort string) (present bool, err error) {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", probeScript()}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("probe dockerd: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !dockerdPresent(string(out)) {
		return false, nil
	}
	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", configureScript(mirrorURL, hostPort)}).CombinedOutput()
	if err != nil {
		return true, fmt.Errorf("configure docker registry mirror: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// ConfigureInstanceDockerPolling points an instance's dockerd at the fleet's
// shared image cache, tolerating a docker-in-docker daemon that has not finished
// starting yet. It probes on a fixed schedule (pollInterval, up to pollTimeout):
//
//   - The first pollSyncAttempts probes run SYNCHRONOUSLY on the caller's
//     goroutine, so a dind that comes up quickly — the common case — is wired up
//     before the user starts working, keeping the mirror reload out of an active
//     workflow.
//   - If dockerd has still not appeared, the remaining probes run in a BACKGROUND
//     goroutine ("rescue mode") until pollTimeout from the first probe. A reload
//     there may land mid-workflow, which is acceptable for the rescue path.
//
// onResult is invoked exactly once with the terminal outcome: configured=true once
// a dockerd was found (err is any configure error), or configured=false if none
// appeared within the window (err is the last probe error, if any). It may run on
// the caller's goroutine (a synchronous hit) or the background goroutine, so it
// must be safe to call from either. Callers gate this on the image-cache feature
// being enabled.
func ConfigureInstanceDockerPolling(b instanceExecer, workspaceDir, mirrorURL, hostPort string, onResult func(configured bool, err error)) {
	start := time.Now()
	var lastErr error

	// Synchronous phase.
	for i := range pollSyncAttempts {
		if i > 0 {
			time.Sleep(pollInterval)
		}
		present, err := tryConfigureOnce(b, workspaceDir, mirrorURL, hostPort)
		if present {
			onResult(true, err)
			return
		}
		if err != nil {
			lastErr = err
		}
	}

	// Background "rescue" phase.
	go func() {
		deadline := start.Add(pollTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(pollInterval)
			present, err := tryConfigureOnce(b, workspaceDir, mirrorURL, hostPort)
			if present {
				onResult(true, err)
				return
			}
			if err != nil {
				lastErr = err
			}
		}
		onResult(false, lastErr)
	}()
}

// probeScript reports whether the instance runs its own dockerd (docker-in-docker)
// that we can reconfigure: a `dockerd` process must be running AND /etc/docker
// must be reachable (writable, absent-but-creatable, or via passwordless sudo).
// docker-outside-of-docker (host socket, no local dockerd) and no-docker images
// yield ABSENT. Always exits 0 and prints exactly one marker.
//
// dockerd is detected by scanning /proc/<pid>/comm rather than `pgrep` so the
// probe works on minimal images that lack procps (/proc/<pid>/comm is
// world-readable, so this works as a non-root user too).
func probeScript() string {
	return fmt.Sprintf(
		`d=0; for c in /proc/[0-9]*/comm; do [ -r "$c" ] && IFS= read -r n < "$c" && [ "$n" = dockerd ] && { d=1; break; }; done; if [ "$d" = 1 ] && { [ -w /etc/docker ] || [ ! -e /etc/docker ] || { command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; }; }; then echo %s; else echo %s; fi`,
		probeMarkerPresent, probeMarkerAbsent,
	)
}

// configureScript merges the registry mirror into /etc/docker/daemon.json and
// SIGHUP-reloads dockerd. registry-mirrors and insecure-registries are both
// SIGHUP-reloadable, so this does NOT restart the daemon (no running containers
// are killed). The merge preserves any existing daemon.json keys: python3 is
// preferred, then jq, then a fresh file only when none exists — it never
// clobbers a file it cannot safely merge. Runs via sudo when /etc/docker is not
// directly writable. mirrorURL/hostPort derive from a sanitized container name,
// so they are shell-safe.
func configureScript(mirrorURL, hostPort string) string {
	const tmpl = `set -e
MIRROR='%[1]s'
INSECURE='%[2]s'
DAEMON=/etc/docker/daemon.json
TMP="$DAEMON.fleet.tmp"
SUDO=
if [ "$(id -u)" != 0 ]; then SUDO=sudo; fi
$SUDO mkdir -p /etc/docker
if command -v python3 >/dev/null 2>&1; then
  # Merge via python: parse the existing file, refuse to clobber one we can't
  # parse, and write atomically (tmp + os.replace).
  $SUDO python3 - "$DAEMON" "$TMP" "$MIRROR" "$INSECURE" <<'PY'
import json, os, sys
path, tmp, mirror, insecure = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
d = {}
if os.path.exists(path):
    with open(path) as f:
        text = f.read().strip()
    if text:
        try:
            d = json.loads(text)
        except ValueError:
            sys.stderr.write("fleet: /etc/docker/daemon.json is not valid JSON; leaving it untouched\n")
            sys.exit(1)
        if not isinstance(d, dict):
            sys.stderr.write("fleet: /etc/docker/daemon.json is not a JSON object; leaving it untouched\n")
            sys.exit(1)
mirrors = d.get("registry-mirrors") or []
if mirror not in mirrors:
    mirrors.append(mirror)
d["registry-mirrors"] = mirrors
insec = d.get("insecure-registries") or []
if insecure not in insec:
    insec.append(insecure)
d["insecure-registries"] = insec
with open(tmp, "w") as f:
    json.dump(d, f, indent=2)
os.replace(tmp, path)
PY
elif command -v jq >/dev/null 2>&1; then
  [ -s "$DAEMON" ] || echo '{}' | $SUDO tee "$DAEMON" >/dev/null
  # Capture jq's output so a parse failure is detected here (a pipe to tee would
  # mask jq's non-zero exit) and never clobbers the original. Write atomically.
  if MERGED=$($SUDO jq --arg m "$MIRROR" --arg i "$INSECURE" '."registry-mirrors" = ((."registry-mirrors" // []) + [$m] | unique) | ."insecure-registries" = ((."insecure-registries" // []) + [$i] | unique)' "$DAEMON"); then
    printf '%%s\n' "$MERGED" | $SUDO tee "$TMP" >/dev/null && $SUDO mv "$TMP" "$DAEMON"
  else
    echo "fleet: jq could not parse /etc/docker/daemon.json; leaving it untouched" >&2
    exit 1
  fi
elif [ ! -s "$DAEMON" ]; then
  printf '{\n  "registry-mirrors": ["%%s"],\n  "insecure-registries": ["%%s"]\n}\n' "$MIRROR" "$INSECURE" | $SUDO tee "$DAEMON" >/dev/null
else
  exit 0
fi
# Reload dockerd so it picks up registry-mirrors + insecure-registries (both are
# SIGHUP-reloadable, so no container-killing restart). Find the pid via /proc so
# this works without procps/pgrep.
for d in /proc/[0-9]*; do
  if [ -r "$d/comm" ] && IFS= read -r n < "$d/comm" && [ "$n" = dockerd ]; then
    $SUDO kill -HUP "${d##*/}" 2>/dev/null || true
    break
  fi
done
`
	return fmt.Sprintf(tmpl, mirrorURL, hostPort)
}

// dockerdPresent reports whether the probe output indicates a configurable
// in-instance dockerd. It scans for the PRESENT marker as a substring so leading
// warnings on the combined stream don't mask a positive result.
func dockerdPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}
