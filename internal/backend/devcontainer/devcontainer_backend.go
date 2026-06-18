package devcontainer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// DevcontainerBackend implements backend.Backend using the devcontainer
// and docker CLIs.
type DevcontainerBackend struct {
	verbose bool

	userCache   map[string]string
	userCacheMu sync.Mutex
}

// New creates a new DevcontainerBackend.
func New(opts ...Option) *DevcontainerBackend {
	devcontainerBackend := &DevcontainerBackend{userCache: make(map[string]string)}
	for _, opt := range opts {
		opt(devcontainerBackend)
	}
	return devcontainerBackend
}

// containerUser returns the non-root user inside the container, caching
// the result to avoid a docker exec round-trip on every poll. Only a
// non-empty answer is cached: an empty probe (a just-started container with
// no tmux socket and no /home/<user> yet, or a transient exec failure) must be
// re-probed next call so it self-heals. Caching "" would, now that the backend
// is reused across poll ticks, pin every later exec to root forever and hide
// the real user's tmux sessions.
func (devcontainerBackend *DevcontainerBackend) containerUser(containerID string) string {
	devcontainerBackend.userCacheMu.Lock()
	if cached, ok := devcontainerBackend.userCache[containerID]; ok {
		devcontainerBackend.userCacheMu.Unlock()
		return cached
	}
	devcontainerBackend.userCacheMu.Unlock()

	// Pick the owner of an active tmux server socket directory.
	// tmux creates /tmp/tmux-$UID/ for each user that runs a server,
	// so this is the canonical signal for "which user holds fleet's
	// sessions". The [1-9]* glob skips /tmp/tmux-0/ — a stale dir
	// some images leave behind from a root-side init poke at tmux —
	// because fleet's sessions never live under root. Fall back to
	// the first /home/<user>/ owner when no tmux socket exists yet
	// (brand-new container, first poll before any session).
	cmd := exec.Command("docker", "exec", containerID, "sh", "-c",
		`for d in /tmp/tmux-[1-9]*/; do [ -d "$d" ] && stat -c %U "$d" 2>/dev/null && exit; done; `+
			`for d in /home/*/; do [ -d "$d" ] && stat -c %U "$d" 2>/dev/null && exit; done`)
	out, err := cmd.Output()
	user := ""
	if err == nil {
		user = strings.TrimSpace(string(out))
	}

	// Cache only a real answer; leave a miss uncached so the next poll re-probes.
	if user != "" {
		devcontainerBackend.userCacheMu.Lock()
		devcontainerBackend.userCache[containerID] = user
		devcontainerBackend.userCacheMu.Unlock()
	}
	return user
}

// Up provisions a devcontainer for the given workspace folder.
func (devcontainerBackend *DevcontainerBackend) Up(workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return devcontainerBackend.up(workspaceDir, mounts, nil)
}

// Rebuild recreates the workspace's container in place by re-running
// `devcontainer up --remove-existing-container`: the existing container is
// removed and a fresh one is provisioned from the (possibly edited)
// devcontainer.json / Dockerfile. The host-side workspace bind mount is
// preserved, so uncommitted work in the checkout survives; the new container
// gets a new ID, returned in UpResult. containerID is unused — the devcontainer
// CLI matches the container by workspace-folder label (and up's
// pruneStaleContainers already drops it), so --remove-existing-container is the
// belt-and-braces signal of rebuild intent. A future "full" rebuild could add
// --build-no-cache to force a from-scratch image build.
func (devcontainerBackend *DevcontainerBackend) Rebuild(_, workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return devcontainerBackend.up(workspaceDir, mounts, []string{"--remove-existing-container"})
}

// SupportsRebuild reports true: the devcontainer backend can recreate a
// container in place from its workspace folder.
func (devcontainerBackend *DevcontainerBackend) SupportsRebuild() bool {
	return true
}

// up runs `devcontainer up` for the given workspace folder. Any mounts
// in the slice are translated into `--mount type=bind,source=L,target=C`
// flags so they appear inside the container alongside the SSH agent
// socket and the workspace itself. extraArgs are passed through verbatim to
// the `up` subcommand (Rebuild uses it for --remove-existing-container).
//
// When the workspace's devcontainer.json declares its own mount at the
// same container path as one of the fleet-supplied mounts, the user's
// entry is temporarily stripped from the file before launching the
// devcontainer CLI and restored once up returns. Without this, docker
// rejects the resulting `docker run` invocation with "Duplicate mount
// point" and the instance enters StatusFailed. Fleet's mount wins by
// design — the user's per-fleet agent state should override whatever
// the repo's devcontainer.json was pointing at.
func (devcontainerBackend *DevcontainerBackend) up(workspaceDir string, mounts []backend.Mount, extraArgs []string) (*backend.UpResult, error) {
	// Drop any docker container still labelled with this workspace
	// folder before provisioning. The devcontainer CLI matches existing
	// containers by `devcontainer.local_folder=<wsDir>` and silently
	// reuses one when found — if a previous instance under the same
	// name left a container behind (failed Down, mid-creation crash,
	// hand-edited state), reuse binds the new instance to a container
	// whose workspace mount no longer maps anywhere, and subsequent
	// `devcontainer exec` calls fail with runc's "current working
	// directory is outside of container mount namespace root" check.
	devcontainerBackend.pruneStaleContainers(workspaceDir)

	restore, err := neutralizeConflictingMounts(workspaceDir, mounts)
	if err != nil {
		return nil, err
	}
	defer restore()

	// Isolate the devcontainer CLI's scratch directory for this invocation.
	// The CLI materialises its generated `updateUID.Dockerfile-<ver>` under a
	// FIXED name in `$TMPDIR/devcontainercli-runner/`, writing a temp sibling
	// and renaming it onto that path. Two concurrent `devcontainer up`
	// processes that share one TMPDIR therefore collide on the rename and one
	// dies with `ENOENT ... rename .../updateUID.Dockerfile-<ver>`. A
	// per-invocation TMPDIR gives each process its own runner dir, so
	// concurrent ups never race (guarded by integration test
	// 600_concurrent_up_distinct, the #63 regression).
	runnerTmp, err := os.MkdirTemp("", "fleet-devcontainer-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(runnerTmp)

	args := []string{"up", "--workspace-folder", workspaceDir}
	args = append(args, extraArgs...)
	args, err = devcontainerUpArgs(args)
	if err != nil {
		return nil, err
	}
	args = append(args, sshUpArgs()...)
	args = append(args, customMountArgs(mounts)...)
	cmd := exec.Command("devcontainer", args...)
	env, err := devcontainerEnv(os.Environ())
	if err != nil {
		return nil, err
	}
	cmd.Env = withIsolatedTmp(env, runnerTmp)

	// Capture stdout for JSON parsing, tee to os.Stdout so it appears
	// in log files when run from the TUI background process.
	var stdoutBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)

	// Always tee stderr to os.Stderr so build output reaches the log
	// file. A buffer captures it for inclusion in error messages.
	var stderrBuf strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderrBuf.String())
		if detail != "" {
			return nil, fmt.Errorf("devcontainer up failed: %w\n%s", err, detail)
		}
		return nil, fmt.Errorf("devcontainer up failed: %w", err)
	}

	var result backend.UpResult
	if err := json.Unmarshal(stdoutBuf.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse devcontainer up output: %w\nraw: %s", err, stdoutBuf.String())
	}

	if result.Outcome != "success" {
		return nil, fmt.Errorf("devcontainer up returned outcome %q", result.Outcome)
	}

	return &result, nil
}

// devcontainerUpArgs appends the optional --update-remote-user-uid-default
// flag based on the FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID env var.
func devcontainerUpArgs(base []string) ([]string, error) {
	mode := strings.TrimSpace(os.Getenv("FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID"))
	switch mode {
	case "", "default":
		return base, nil
	case "never", "on", "off":
		return append(base, "--update-remote-user-uid-default", mode), nil
	default:
		return nil, fmt.Errorf("invalid FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID value %q (valid: default, never, on, off)", mode)
	}
}

// withIsolatedTmp returns env with TMPDIR pointed at dir, replacing any
// inherited TMPDIR. The devcontainer CLI (Node's os.tmpdir()) reads TMPDIR
// first on Unix, so this redirects its `devcontainercli-runner` scratch into a
// per-invocation directory — see Up for why that race-proofing matters.
func withIsolatedTmp(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "TMPDIR=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "TMPDIR="+dir)
}

// devcontainerEnv applies BuildKit-related env tweaks based on the
// FLEET_DEVCONTAINER_BUILDKIT env var.
func devcontainerEnv(base []string) ([]string, error) {
	mode := strings.TrimSpace(os.Getenv("FLEET_DEVCONTAINER_BUILDKIT"))
	switch mode {
	case "", "auto":
		return base, nil
	case "never":
		return append(base, "DOCKER_BUILDKIT=0"), nil
	default:
		return nil, fmt.Errorf("invalid FLEET_DEVCONTAINER_BUILDKIT value %q (valid: auto, never)", mode)
	}
}

// Exec runs `devcontainer exec` in the given workspace folder, logging a
// single timed "container exec" event when it completes.
func (devcontainerBackend *DevcontainerBackend) Exec(workspaceDir string, command []string) error {
	cmd := devcontainerBackend.rawExec(workspaceDir, command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := cmd.Run()
	flog.ContainerExec("devcontainer", workspaceDir, command, time.Since(start), err)
	return err
}

// ExecCommand returns an unstarted *backend.Cmd for running a command inside
// a workspace via `devcontainer exec`. Because *Cmd shadows the run methods,
// it logs a single timed "container exec" event when the caller runs it via
// Run/Output/CombinedOutput. Hot polling loops should use ExecCommandQuiet.
func (devcontainerBackend *DevcontainerBackend) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(devcontainerBackend.rawExec(workspaceDir, command), func(d time.Duration, err error) {
		flog.ContainerExec("devcontainer", workspaceDir, command, d, err)
	})
}

// ExecCommandQuiet is ExecCommand without any event-log entries.
func (devcontainerBackend *DevcontainerBackend) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(devcontainerBackend.rawExec(workspaceDir, command), nil)
}

// rawExec builds the underlying `devcontainer exec` *exec.Cmd shared by
// ExecCommand and ExecCommandQuiet.
func (devcontainerBackend *DevcontainerBackend) rawExec(workspaceDir string, command []string) *exec.Cmd {
	args := execArgs(workspaceDir, command)
	return exec.Command("devcontainer", args...)
}

// pruneStaleContainers force-removes any docker container labelled
// `devcontainer.local_folder=<workspaceDir>`. Called from Up() so that
// reusing an instance name (same wsDir on disk) never silently rebinds
// to a leftover container with a broken workspace mount. Errors are
// logged but non-fatal: if cleanup genuinely fails, the subsequent
// `devcontainer up` will surface a real error.
func (devcontainerBackend *DevcontainerBackend) pruneStaleContainers(workspaceDir string) {
	if workspaceDir == "" {
		return
	}
	listCmd := exec.Command("docker", "ps", "-a", "-q",
		"--filter", "label=devcontainer.local_folder="+workspaceDir)
	out, err := listCmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to list stale containers for %s: %v\n", workspaceDir, err)
		return
	}
	for _, id := range strings.Fields(string(out)) {
		rmCmd := exec.Command("docker", "rm", "-f", id)
		if rmOut, err := rmCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove stale container %s: %v\n%s\n",
				id, err, strings.TrimSpace(string(rmOut)))
		}
	}
}

// Down stops and removes a container by ID using docker directly.
// If the container was produced by Clone (carries the fleet.clone.image
// label), the committed image is removed afterwards so cloned-instance
// teardown does not leak intermediate images. Image-rm failures are
// logged but non-fatal — the container is already gone, which is what
// callers actually need.
func (devcontainerBackend *DevcontainerBackend) Down(containerID string) error {
	cloneImage := cloneImageForContainer(containerID)
	// Capture the container's volumes before removing it — `docker inspect`
	// no longer reports them once the container is gone.
	volumes := volumesForContainer(containerID)

	cmd := exec.Command("docker", "rm", "-f", containerID)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	// Remove the volumes the container mounted. `docker rm` leaves named
	// volumes behind, so an instance whose devcontainer declared a
	// `type=volume` mount (or pulled in a feature like docker-in-docker)
	// would otherwise leak a volume on every teardown. Docker refuses to
	// delete a volume still referenced by another container, so anything
	// shared with a sibling instance survives — only this instance's own
	// volumes go. Best-effort: a failure leaks a volume but the container
	// (what callers need gone) is already removed.
	for _, vol := range volumes {
		rmCmd := exec.Command("docker", "volume", "rm", vol)
		if rmOut, err := rmCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove volume %s: %v\n%s\n",
				vol, err, strings.TrimSpace(string(rmOut)))
		}
	}

	if cloneImage != "" {
		rmiCmd := exec.Command("docker", "rmi", cloneImage)
		if rmiOut, err := rmiCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove clone image %s: %v\n%s\n",
				cloneImage, err, strings.TrimSpace(string(rmiOut)))
		}
	}
	return nil
}

// Stop stops a container by ID without removing it.
func (devcontainerBackend *DevcontainerBackend) Stop(containerID string) error {
	cmd := exec.Command("docker", "stop", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start starts an existing container by ID and then fires the devcontainer
// postStartCommand lifecycle hook so a resumed instance behaves like one
// brought up by any other devcontainer tool (issue #179). A bare `docker
// start` leaves postStartCommand un-run because docker knows nothing about
// devcontainer lifecycle, so we follow it with `devcontainer
// run-user-commands` (see runPostStart). The post-start step is best-effort:
// the container is already running, so a hook failure is logged rather than
// reverting the instance to stopped and desyncing fleet's state from docker.
func (devcontainerBackend *DevcontainerBackend) Start(containerID string) error {
	cmd := exec.Command("docker", "start", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	devcontainerBackend.runPostStart(containerID)
	return nil
}

// runPostStart re-runs the devcontainer postStartCommand (and postAttachCommand)
// for a just-started container via `devcontainer run-user-commands`. That
// subcommand re-runs only the lifecycle commands whose markers are stale
// relative to the container's start time — postStart and postAttach — while
// leaving the create-time commands (onCreate/updateContent/postCreate)
// untouched, because their markers are pinned to the unchanged container
// creation time. It never recreates the container, so the instance keeps its
// identity (unlike `up`, which could rebuild on config drift).
//
// The workspace folder is recovered from the container's
// `devcontainer.local_folder` label — the same label the CLI stamps at `up`
// time — so Start needs only the container ID. Best-effort: a missing label or
// a non-zero exit is logged (output already teed to fleet.log) and swallowed,
// since the container is running regardless.
func (devcontainerBackend *DevcontainerBackend) runPostStart(containerID string) {
	workspaceDir := workspaceFolderForContainer(containerID)
	if workspaceDir == "" {
		flog.Warn("devcontainer post-start skipped: container has no workspace-folder label", "container", containerID)
		return
	}

	cmd := exec.Command("devcontainer", runUserCommandsArgs(workspaceDir)...)
	// Tee output to the process stdio so the postStart script's output (and
	// any failure detail) reaches the log file under the TUI's background
	// process, exactly like up().
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		flog.Warn("devcontainer post-start failed", "container", containerID, "workspace", workspaceDir, "ms", flog.MillisSince(start), "err", err)
		return
	}
	flog.Info("devcontainer post-start ran", "container", containerID, "workspace", workspaceDir, "ms", flog.MillisSince(start))
}

// runUserCommandsArgs builds the `devcontainer run-user-commands` argument list
// for a just-started workspace, threading the SSH agent socket through with
// --remote-env so a postStart hook that talks to git/ssh sees the same agent
// the workspace was created with — mirroring devcontainer exec.
func runUserCommandsArgs(workspaceDir string) []string {
	args := []string{"run-user-commands", "--workspace-folder", workspaceDir}
	args = append(args, sshExecArgs()...)
	return args
}

// workspaceFolderForContainer returns the host workspace folder a container was
// provisioned from, read from the `devcontainer.local_folder` label the CLI
// stamps at `up` time (and that Clone re-applies to cloned containers). Returns
// "" when the inspect fails or the label is absent.
func workspaceFolderForContainer(containerID string) string {
	inspected, err := inspectContainer(containerID)
	if err != nil || inspected.Config.Labels == nil {
		return ""
	}
	return inspected.Config.Labels[devcontainerLocalFolderLabel]
}

// Stats returns CPU and memory usage for the given container IDs.
func (devcontainerBackend *DevcontainerBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	if len(containerIDs) == 0 {
		return nil, nil
	}

	args := append([]string{"stats", "--no-stream", "--format", "{{.ID}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, containerIDs...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	result := make(map[string]*backend.ContainerStats)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}

		id := fields[0]
		cpuStr := strings.TrimSuffix(strings.TrimSpace(fields[1]), "%")
		cpuPct, _ := backend.ParseFloat(cpuStr)
		cpuMillicores := cpuPct * 10

		memParts := strings.SplitN(fields[2], "/", 2)
		memMB := parseMemToMB(strings.TrimSpace(memParts[0]))

		for _, requestedID := range containerIDs {
			if strings.HasPrefix(requestedID, id) || strings.HasPrefix(id, requestedID) {
				result[requestedID] = &backend.ContainerStats{
					CPUMillicores: cpuMillicores,
					MemoryMB:      memMB,
				}
			}
		}
	}

	return result, nil
}

// Logs streams docker logs for a container.
func (devcontainerBackend *DevcontainerBackend) Logs(containerID string, follow bool) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// LogsCommand returns an unstarted *exec.Cmd for streaming container logs.
func (devcontainerBackend *DevcontainerBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)
	return exec.Command("docker", args...)
}

// CaptureAllSessions lists and captures every tmux session inside a
// container in one docker exec call.
func (devcontainerBackend *DevcontainerBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	args := []string{"exec"}
	if user := devcontainerBackend.containerUser(containerID); user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, containerID, "sh", "-c", backend.CaptureAllScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	sessions, files, hookMissing := backend.ParseCaptureOutput(string(out))
	return backend.AllSessions{
		Sessions:          sessions,
		ExtraFiles:        files,
		HookScriptMissing: hookMissing,
		OK:                true,
	}
}

// ListSessions lists tmux sessions inside the container with a direct
// `docker exec`, bypassing the devcontainer CLI's Node cold start (the session
// poller runs every second, where that cold start dominated CPU). It runs as
// the container's non-root user — the owner of fleet's tmux sessions — reusing
// the cached user lookup, exactly as CaptureAllSessions does. Returns "" on
// exec failure (e.g. no tmux server), which the caller reads as "no sessions".
func (devcontainerBackend *DevcontainerBackend) ListSessions(containerID string) string {
	args := []string{"exec"}
	if user := devcontainerBackend.containerUser(containerID); user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, containerID, "sh", "-c", backend.ListSessionsScript)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// AgentToolProbe detects which agent tool is running inside a container.
func (devcontainerBackend *DevcontainerBackend) AgentToolProbe(containerID string) (string, bool) {
	args := []string{"exec"}
	if user := devcontainerBackend.containerUser(containerID); user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, containerID, "sh", "-c", backend.ToolProbeScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return backend.ParseToolProbeOutput(string(out))
}

// SupportsCustomMounts reports that the devcontainer backend honors
// caller-supplied bind mounts via `devcontainer up --mount` flags.
func (devcontainerBackend *DevcontainerBackend) SupportsCustomMounts() bool {
	return true
}

// customMountArgs translates a slice of backend.Mount values into the
// `--mount type=bind,...` flag pairs accepted by `devcontainer up`.
// Returns nil for an empty input so callers can append unconditionally.
func customMountArgs(mounts []backend.Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	args := make([]string, 0, len(mounts)*2)
	for _, mount := range mounts {
		if mount.LocalPath == "" || mount.ContainerPath == "" {
			continue
		}
		args = append(args, "--mount", "type=bind,source="+mount.LocalPath+",target="+mount.ContainerPath)
	}
	return args
}

// EditorURI returns a VS Code devcontainer remote URI.
func (devcontainerBackend *DevcontainerBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	hexPath := hex.EncodeToString([]byte(workspaceDir))
	uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s", hexPath, projectName)
	return uri, true
}

// PortForwardCommand returns an unstarted *exec.Cmd that forwards localPort
// on the host to remotePort inside the container using socat via docker exec.
func (devcontainerBackend *DevcontainerBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	script := fmt.Sprintf(
		`socat TCP-LISTEN:%d,fork,reuseaddr EXEC:"docker exec -i %s socat STDIO TCP\\:localhost\\:%d"`,
		localPort, containerID, remotePort,
	)
	return exec.Command("sh", "-c", script)
}

// ForwardStdioCommand returns an unstarted *exec.Cmd that bridges one
// connection to remotePort inside the container: socat pipes the docker
// exec's stdin/stdout to a TCP connection opened from within the
// container's own network namespace, so it works regardless of whether
// the container's IP is reachable from the host.
func (devcontainerBackend *DevcontainerBackend) ForwardStdioCommand(containerID string, remotePort int) (*exec.Cmd, bool) {
	return exec.Command("docker", "exec", "-i", containerID,
		"socat", "STDIO", fmt.Sprintf("TCP:localhost:%d", remotePort)), true
}

// Status reports the live state of a docker container by reading
// `docker inspect --format {{.State.Status}}`. Maps docker's
// fine-grained states into the coarser backend.LiveStatus enum:
// running/restarting → running; exited/paused/dead/created → stopped;
// "No such container" → missing; everything else (and probe errors)
// → unknown so callers preserve whatever was persisted.
func (devcontainerBackend *DevcontainerBackend) Status(containerID string) backend.LiveStatus {
	if containerID == "" {
		return backend.LiveStatusUnknown
	}
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerID)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if strings.Contains(string(exitErr.Stderr), "No such container") {
				return backend.LiveStatusMissing
			}
		}
		return backend.LiveStatusUnknown
	}
	switch strings.TrimSpace(string(out)) {
	case "running", "restarting":
		return backend.LiveStatusRunning
	case "exited", "paused", "dead", "created":
		return backend.LiveStatusStopped
	case "removing":
		return backend.LiveStatusMissing
	default:
		return backend.LiveStatusUnknown
	}
}

// ResolveHostname returns the container's IP address obtained via
// `docker inspect`. For local Docker containers this is directly reachable.
func (devcontainerBackend *DevcontainerBackend) ResolveHostname(containerID string) (string, bool) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerID,
	).Output()
	if err != nil {
		return "", false
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", false
	}
	return ip, true
}
