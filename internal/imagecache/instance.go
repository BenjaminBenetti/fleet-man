package imagecache

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// extra warnings on stderr. probeMarkerDenied is a pod engine (below) whose
// daemon.json the exec user cannot write.
const (
	probeMarkerPresent = "PRESENT"
	probeMarkerAbsent  = "ABSENT"
	probeMarkerDenied  = "DENIED"
)

// A docker-outside-of-docker instance on a cluster has its own docker engine:
// the pod's fleet-docker sidecar (podspec.DockerDir), a private engine that
// would otherwise pull every image again, so it gets the mirror too (on a
// docker host such an instance shares the host's engine and is left alone).
// The dev container sees no dockerd process (the sidecar is another
// container, with no shared PID namespace); it finds the engine by
// DockerdConfigEnv, which names the daemon.json the engine reloads by itself
// whenever it changes, or else by the engine's socket. The folder is root's,
// as /etc/docker is.
const (
	// DockerdConfigEnv names, in the dev container, the pod engine's
	// daemon.json (podspec.DockerConfigEnv).
	DockerdConfigEnv = "FLEET_DOCKERD_CONFIG"
	// PodEngineSocket and PodEngineConfig are the engine's socket and
	// daemon.json (podspec.DockerSocket, podspec.DockerConfig).
	PodEngineSocket = "/opt/fleet/docker/docker.sock"
	PodEngineConfig = "/opt/fleet/docker/daemon.json"
)

// errPodEngineDenied is a pod engine whose daemon.json the instance's user
// may not replace and cannot sudo to.
var errPodEngineDenied = fmt.Errorf("the pod's docker engine reads its daemon.json (%s) from a folder only root may write, and the instance's user has no passwordless sudo: its pulls do not use the mirror", PodEngineConfig)

// ConfigureInstanceDocker points an instance's OWN docker daemon at the fleet's
// shared image cache by adding registry-mirrors + insecure-registries to
// /etc/docker/daemon.json and reloading dockerd, or to the pod engine's
// daemon.json, which that engine reloads itself. It first probes for a LOCAL
// dockerd (docker-in-docker) or a pod engine; if there is none —
// docker-outside-of-docker on a docker host, or no docker — it returns nil
// WITHOUT touching the instance (the documented "do nothing, no error"
// behaviour). The daemon.json merge is itself best-effort and never clobbers an
// existing file it can't safely merge.
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
	if strings.Contains(string(out), probeMarkerDenied) {
		// An engine is there, and no later probe will find it writable.
		return true, errPodEngineDenied
	}
	if !dockerdPresent(string(out)) {
		return false, nil
	}
	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", configureScript(mirrorURL, hostPort)}).CombinedOutput()
	if err != nil {
		return true, fmt.Errorf("configure docker registry mirror: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if strings.Contains(string(out), mergeMarkerNoTool) {
		// No python3 or jq to merge with in the instance (a docker:dind
		// image has neither), and a daemon.json with keys of its own (on a
		// cluster pod-init writes the engine's MTU there): fleet merges it.
		if err := mergeHere(b, workspaceDir, mirrorURL, hostPort); err != nil {
			return true, fmt.Errorf("configure docker registry mirror: %w", err)
		}
	}
	return true, nil
}

// mergeMarkerNoTool is what configureScript prints when it has nothing to
// merge the daemon.json with, leaving the merge to mergeHere.
const mergeMarkerNoTool = "FLEET_MERGE_NO_TOOL"

// mergeHere merges the mirror into the daemon's daemon.json on fleet's side:
// it reads the file out of the instance, merges it as the script's python
// does, and writes it back (by temp file and rename) and reloads the daemon,
// unless the mirror is in it already.
func mergeHere(b instanceExecer, workspaceDir, mirrorURL, hostPort string) error {
	cur, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", readConfigScript()}).Output()
	if err != nil {
		return fmt.Errorf("read the daemon's daemon.json: %w", err)
	}
	merged, changed, err := mergeMirror(cur, mirrorURL, hostPort)
	if err != nil || !changed {
		return err
	}
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", writeConfigScript(merged)}).CombinedOutput(); err != nil {
		return fmt.Errorf("write the daemon's daemon.json: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mergeMirror is daemon.json data with mirrorURL in its registry-mirrors and
// hostPort in its insecure-registries, every other key kept; changed is
// false when both are there already. Data that is not a JSON object is an
// error: the file is left as it is.
func mergeMirror(data []byte, mirrorURL, hostPort string) ([]byte, bool, error) {
	conf := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &conf); err != nil || conf == nil {
			return nil, false, errors.New("the daemon's daemon.json is not a JSON object; leaving it untouched")
		}
	}
	changed := false
	for key, v := range map[string]string{"registry-mirrors": mirrorURL, "insecure-registries": hostPort} {
		list, _ := conf[key].([]any)
		if _, isList := conf[key].([]any); conf[key] != nil && !isList {
			return nil, false, fmt.Errorf("the daemon's daemon.json has a %s that is not a list; leaving it untouched", key)
		}
		if !slices.Contains(list, any(v)) {
			conf[key] = append(list, v)
			changed = true
		}
	}
	if !changed {
		return data, false, nil
	}
	out, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
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

// probeScript reports whether the instance has a docker daemon we can
// reconfigure: a pod engine (FLEET_DOCKERD_CONFIG set, or its socket there)
// whose folder is writable directly or via passwordless sudo (DENIED when it
// is not), or its own dockerd (docker-in-docker): a `dockerd` process must be
// running AND /etc/docker must be reachable (writable, absent-but-creatable,
// or via passwordless sudo). docker-outside-of-docker on a docker host (the
// host's socket, no local dockerd) and no-docker images yield ABSENT. Always
// exits 0 and prints exactly one marker.
//
// dockerd is detected by scanning /proc/<pid>/comm rather than `pgrep` so the
// probe works on minimal images that lack procps (/proc/<pid>/comm is
// world-readable, so this works as a non-root user too).
func probeScript() string {
	return fmt.Sprintf(
		`cfg=${%[4]s:-}; if [ -z "$cfg" ] && [ -S %[5]s ]; then cfg=%[6]s; fi; if [ -n "$cfg" ]; then if [ -w "${cfg%%/*}" ] || { command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; }; then echo %[1]s; else echo %[3]s; fi; exit 0; fi; `+
			`d=0; for c in /proc/[0-9]*/comm; do [ -r "$c" ] && IFS= read -r n < "$c" && [ "$n" = dockerd ] && { d=1; break; }; done; if [ "$d" = 1 ] && { [ -w /etc/docker ] || [ ! -e /etc/docker ] || { command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; }; }; then echo %[1]s; else echo %[2]s; fi`,
		probeMarkerPresent, probeMarkerAbsent, probeMarkerDenied, DockerdConfigEnv, PodEngineSocket, PodEngineConfig,
	)
}

// configureScript merges the registry mirror into the daemon's daemon.json and
// has the daemon reload it. registry-mirrors and insecure-registries are both
// SIGHUP-reloadable, so this does NOT restart the daemon (no running containers
// are killed). The instance's own dockerd reads /etc/docker/daemon.json and is
// sent SIGHUP here; a pod engine's file is the one FLEET_DOCKERD_CONFIG names
// (or the one beside its socket), and the engine, a process this container
// cannot see, reloads it itself once it changes. The merge preserves any
// existing daemon.json keys: python3 is preferred, then jq, then a fresh file
// only when none exists or it holds an empty object — it never clobbers a file
// it cannot safely merge. Every write is a temp file renamed over the old one,
// so a reload never reads half a file. Runs via sudo when not root.
// mirrorURL/hostPort derive from a sanitized name, so they are shell-safe.
func configureScript(mirrorURL, hostPort string) string {
	const tmpl = `set -e
MIRROR='%[1]s'
INSECURE='%[2]s'
%[3]s$SUDO mkdir -p "${DAEMON%%/*}"
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
            sys.stderr.write("fleet: %%s is not valid JSON; leaving it untouched\n" %% path)
            sys.exit(1)
        if not isinstance(d, dict):
            sys.stderr.write("fleet: %%s is not a JSON object; leaving it untouched\n" %% path)
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
    echo "fleet: jq could not parse $DAEMON; leaving it untouched" >&2
    exit 1
  fi
elif [ ! -s "$DAEMON" ] || [ "$($SUDO cat "$DAEMON" 2>/dev/null | tr -d ' \t\r\n')" = '{}' ]; then
  # Nothing to keep: no file, an empty one, or an empty object (what the
  # pod's engine starts with).
  printf '{\n  "registry-mirrors": ["%%s"],\n  "insecure-registries": ["%%s"]\n}\n' "$MIRROR" "$INSECURE" | $SUDO tee "$TMP" >/dev/null
  $SUDO mv "$TMP" "$DAEMON"
else
  # Keys to keep and nothing to merge them with: fleet merges it (mergeHere).
  echo %[4]s
  exit 0
fi
%[5]s`
	return fmt.Sprintf(tmpl, mirrorURL, hostPort, daemonPrelude(), mergeMarkerNoTool, reloadScript)
}

// daemonPrelude finds the daemon.json to configure (DAEMON; RELOADS_ITSELF
// for the pod's engine, which reloads its own; the instance's dockerd is
// sent SIGHUP), its temp file and the sudo it takes.
func daemonPrelude() string {
	return fmt.Sprintf(`DAEMON=${%[1]s:-}
if [ -z "$DAEMON" ] && [ -S %[2]s ]; then DAEMON=%[3]s; fi
RELOADS_ITSELF=
[ -z "$DAEMON" ] || RELOADS_ITSELF=1
DAEMON=${DAEMON:-/etc/docker/daemon.json}
TMP="$DAEMON.fleet.tmp"
SUDO=
if [ "$(id -u)" != 0 ]; then SUDO=sudo; fi
`, DockerdConfigEnv, PodEngineSocket, PodEngineConfig)
}

// reloadScript has the instance's dockerd reload its daemon.json:
// registry-mirrors + insecure-registries are both SIGHUP-reloadable, so no
// container-killing restart. It finds the pid via /proc so it works without
// procps/pgrep. The pod's engine reloads by itself.
const reloadScript = `[ -z "$RELOADS_ITSELF" ] || exit 0
for d in /proc/[0-9]*; do
  if [ -r "$d/comm" ] && IFS= read -r n < "$d/comm" && [ "$n" = dockerd ]; then
    $SUDO kill -HUP "${d##*/}" 2>/dev/null || true
    break
  fi
done
`

// readConfigScript prints the daemon's daemon.json (nothing when there is
// none).
func readConfigScript() string {
	return "set -e\n" + daemonPrelude() + `if [ -e "$DAEMON" ]; then $SUDO cat -- "$DAEMON"; fi
`
}

// writeConfigScript replaces the daemon's daemon.json with content (base64
// in the script, so any content is shell-safe), by temp file and rename, and
// has the daemon reload it.
func writeConfigScript(content []byte) string {
	return "set -e\n" + daemonPrelude() + fmt.Sprintf(`$SUDO mkdir -p "${DAEMON%%/*}"
printf '%%s' '%s' | base64 -d | $SUDO tee "$TMP" >/dev/null
$SUDO mv -f "$TMP" "$DAEMON"
`, base64.StdEncoding.EncodeToString(content)) + reloadScript
}

// dockerdPresent reports whether the probe output indicates a configurable
// in-instance dockerd. It scans for the PRESENT marker as a substring so leading
// warnings on the combined stream don't mask a positive result.
func dockerdPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}
