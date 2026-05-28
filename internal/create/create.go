package create

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Run performs instance creation for an instance that already exists
// in state.json with StatusCreating. For devcontainer backends it runs
// git clone + devcontainer up. For coder backends it runs coder create.
// On success it updates the instance to StatusRunning. On failure it sets
// StatusFailed with the error message.
//
// When branch is non-empty, the devcontainer clone uses `git clone
// --branch <branch>` so the instance is provisioned against that ref
// rather than the repository's default branch.
func Run(fleetName, instanceName, remoteURL, branch string, verbose bool, backendType fleet.BackendType) error {
	if err := fleet.ValidateBackendType(backendType); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, instanceName, fleetName)

	var instanceBackend backend.Backend
	switch backendType {
	case fleet.BackendCoder:
		instanceBackend = buildCoderBackend(fleetName, instanceName, remoteURL, branch, verbose)
	case fleet.BackendCodespaces:
		instanceBackend = buildCodespacesBackend(remoteURL, branch, verbose)
	default:
		instanceBackend = backendutil.New(backendType, verbose)
	}

	if backendType != fleet.BackendCoder && backendType != fleet.BackendCodespaces {
		// Devcontainer: clone repo first, then provision
		if err := os.MkdirAll(filepath.Dir(wsDir), 0755); err != nil {
			setFailed(fleetName, instanceName, err)
			return fmt.Errorf("mkdir: %w", err)
		}

		cloneArgs := []string{"clone"}
		if branch != "" {
			cloneArgs = append(cloneArgs, "--branch", branch)
		}
		cloneArgs = append(cloneArgs, remoteURL, wsDir)
		gitClone := exec.Command("git", cloneArgs...)
		// Tee output to os.Stdout/os.Stderr (the log file when run from
		// the TUI) while capturing it for inclusion in error messages.
		var cloneBuf bytes.Buffer
		gitClone.Stdout = io.MultiWriter(os.Stdout, &cloneBuf)
		gitClone.Stderr = io.MultiWriter(os.Stderr, &cloneBuf)
		if err := gitClone.Run(); err != nil {
			wrapped := fmt.Errorf("git clone failed: %w\n%s", err, cloneBuf.String())
			setFailed(fleetName, instanceName, wrapped)
			return wrapped
		}
	}

	resolvedMounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	result, err := instanceBackend.Up(wsDir, resolvedMounts.Mounts)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Stage the fleet-launch binary into the container so its
	// in-container subcommands (landing-page server, etc.) are ready
	// without waiting for the first browser open. Non-fatal: the
	// browser-open path stages on demand too, so a failure here just
	// defers the work, it doesn't break the instance.
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, wsDir, nil); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet-launch: %v", err))
	}

	// Stage fleet.rc into the container user's home so interactive
	// shells can source fleet-aware aliases. Respects the fleet's
	// HomeDir setting; empty falls back to fleetlaunch.DefaultHomeDir.
	// Non-fatal for the same reason as the binary stage.
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, wsDir, homeDirForFleet(fleetName)); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise the post-Up symlinks for any single-file mounts.
	// Failure is non-fatal — the instance is still usable; the agent
	// will just have to log in fresh — so we surface the error as a
	// warning and keep going.
	if err := applyMountSymlinks(instanceBackend, wsDir, resolvedMounts.Symlinks); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("setting up agentic mount symlinks: %v", err))
	}

	// Auto-install dotfiles. A failure here is non-fatal — the instance
	// is still usable, so we mark it running and surface the error as a
	// warning rather than blocking the whole creation.
	config, _ := state.LoadConfig()
	if config != nil && config.DotfilesSettings.AutoInstall {
		if script := dotfiles.SetupScript(config); script != "" {
			cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", script})
			out, err := cmd.CombinedOutput()
			if err != nil {
				detail := strings.TrimSpace(string(out))
				// Write warning to a file the TUI can pick up.
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("dotfiles install failed: %v\n%s", err, detail))
			}
		}
	}

	// Run any agent install scripts associated with the fleet's
	// settings (Claude Code, Codex). Each script self-redirects its
	// output to ~/.fleet/startup/<name>.log inside the container, and
	// failures are non-fatal — the instance is still usable, so we
	// surface them as a warning rather than aborting creation.
	runStartupScripts(instanceBackend, wsDir, fleetName, instanceName)

	// Install Claude Code state-detection hooks. Runs after the
	// startup scripts so any per-fleet Claude Code install above
	// has had a chance to land before we drop our hooks alongside
	// it. Like dotfiles, this is non-fatal: if hook installation
	// fails the user can still use the instance — fleet-man's
	// per-instance Detector falls back to a safe default for
	// Claude state. We always install (regardless of which agent
	// the user actually runs) because we don't know the choice at
	// creation time, and the cost of unused hooks is negligible.
	executor := agentdetect.NewBackendExecutor(instanceBackend, wsDir)
	if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("claude hook install failed: %v", err))
	}

	// Success: update state (instance is running even if dotfiles failed)
	st, err := state.Load()
	if err != nil {
		return err
	}
	if f, ok := st.Fleets[fleetName]; ok {
		if instance, err := f.GetInstance(instanceName); err == nil {
			instance.ContainerID = result.ContainerID
			instance.Status = fleet.StatusRunning
			instance.Error = ""
		}
	}
	return state.Save(st)
}

// resolveCustomMounts looks up the fleet's persisted settings and
// translates them into a Resolved bundle (mounts + symlinks). Returns
// a zero-value Resolved when the backend does not advertise
// SupportsCustomMounts so callers avoid wasted host-side preparation
// for cloud-managed backends.
//
// State load failures are tolerated: if state.json cannot be read the
// instance is still allowed to provision, just without any custom
// mounts. (The instance record we are about to fill in is loaded from
// the same file in the success path immediately after, which will
// surface a hard error then if the file is truly broken.)
func resolveCustomMounts(instanceBackend backend.Backend, fleetName string) (mountresolver.Resolved, error) {
	if !instanceBackend.SupportsCustomMounts() {
		return mountresolver.Resolved{}, nil
	}
	st, err := state.Load()
	if err != nil {
		return mountresolver.Resolved{}, nil
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return mountresolver.Resolved{}, nil
	}
	return mountresolver.Resolve(fleetName, f.Settings)
}

// applyMountSymlinks runs a shell script inside the just-created
// container to materialise each Symlink in symlinks. The script for
// each link, in order:
//
//  1. Migrates any pre-baked file at the symlink target into the
//     shared host file when the host file is still empty — preserving
//     agent state shipped in the image (e.g. a stub ~/.claude.json).
//  2. Replaces the target with a symlink pointing at the shared file.
//  3. If the shared file is still empty AND the link declares a
//     SeedContent, writes that seed so apps that parse the file on
//     startup see a valid initial value (Claude Code, for instance,
//     crashes if ~/.claude.json is not valid JSON).
func applyMountSymlinks(instanceBackend backend.Backend, wsDir string, symlinks []mountresolver.Symlink) error {
	if len(symlinks) == 0 {
		return nil
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, link := range symlinks {
		fmt.Fprintf(&script, `mkdir -p %s
if [ -e %s ] && [ ! -L %s ] && [ ! -s %s ]; then
  cp %s %s
fi
ln -sf %s %s
`,
			dotfiles.ShQuote(filepath.Dir(link.Target)),
			dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Source),
			dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Source),
			dotfiles.ShQuote(link.Source), dotfiles.ShQuote(link.Target),
		)
		if link.SeedContent != "" {
			fmt.Fprintf(&script, `if [ ! -s %s ]; then
  printf '%%s' %s > %s
fi
`,
				dotfiles.ShQuote(link.Source),
				dotfiles.ShQuote(link.SeedContent),
				dotfiles.ShQuote(link.Source),
			)
		}
	}
	cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", script.String()})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runStartupScripts loads the fleet's settings, picks the matching
// install scripts (Claude Code, Codex, …), and runs each one inside
// the container. Output is captured to ~/.fleet/startup/<name>.log
// inside the instance; per-script failures are aggregated into a
// warning file so the TUI can surface them without marking the
// instance as failed.
//
// State load failures are silently tolerated: if state cannot be read,
// no scripts are run. The caller proceeds to mark the instance running
// regardless — install can be re-attempted by the user via shell after
// the instance comes up.
func runStartupScripts(instanceBackend backend.Backend, wsDir, fleetName, instanceName string) {
	st, err := state.Load()
	if err != nil {
		return
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return
	}
	scripts := startup.ScriptsFor(f.Settings)
	if len(scripts) == 0 {
		return
	}
	failures := startup.Run(instanceBackend, wsDir, scripts)
	if len(failures) == 0 {
		return
	}
	lines := make([]string, 0, len(failures))
	for _, failure := range failures {
		lines = append(lines, failure.Error())
	}
	state.WriteWarn(fleetName, instanceName, strings.Join(lines, "\n"))
}

// buildCoderBackend creates a CoderBackend configured from ~/.fleet/config.json
// with template, preset, and resolved parameter bindings. The branch is
// exposed to template parameters via the ${GIT_BRANCH} substitution so
// Coder templates can clone the requested ref.
func buildCoderBackend(fleetName, instanceName, remoteURL, branch string, verbose bool) backend.Backend {
	opts := []coderbackend.Option{}
	if verbose {
		opts = append(opts, coderbackend.WithVerbose(true))
	}

	config, err := state.LoadConfig()
	if err != nil || config == nil {
		return coderbackend.New(opts...)
	}

	coderSettings := config.CoderSettings
	if coderSettings.Template != "" {
		opts = append(opts, coderbackend.WithTemplate(coderSettings.Template))
	}
	if coderSettings.Preset != "" {
		opts = append(opts, coderbackend.WithPreset(coderSettings.Preset))
	}

	// Resolve parameters with variable substitution
	if len(coderSettings.Parameters) > 0 {
		wsName := fleetName + "-" + instanceName
		resolved := make(map[string]string, len(coderSettings.Parameters))
		for _, param := range coderSettings.Parameters {
			value := param.Value
			if value == "" {
				value = param.DefaultValue
			}
			value = strings.ReplaceAll(value, "${GIT_URL}", remoteURL)
			value = strings.ReplaceAll(value, "${GIT_BRANCH}", branch)
			value = strings.ReplaceAll(value, "${INSTANCE_NAME}", wsName)
			if value != "" {
				resolved[param.Name] = value
			}
		}
		opts = append(opts, coderbackend.WithParameters(resolved))
	}

	return coderbackend.New(opts...)
}

// buildCodespacesBackend creates a CodespacesBackend configured from
// ~/.fleet/config.json with machine type and other preferences. When
// branch is non-empty it is passed to `gh codespace create --branch`
// so the codespace is created from that ref instead of the repo default.
func buildCodespacesBackend(remoteURL, branch string, verbose bool) backend.Backend {
	opts := []codespacesbackend.Option{}
	if verbose {
		opts = append(opts, codespacesbackend.WithVerbose(true))
	}

	// Convert git SSH URL to owner/repo format for gh CLI.
	repo := repoFromRemoteURL(remoteURL)
	if repo != "" {
		opts = append(opts, codespacesbackend.WithRepo(repo))
	}

	if branch != "" {
		opts = append(opts, codespacesbackend.WithBranch(branch))
	}

	config, err := state.LoadConfig()
	if err != nil || config == nil {
		return codespacesbackend.New(opts...)
	}

	codespacesSettings := config.CodespacesSettings
	if codespacesSettings.Machine != "" {
		opts = append(opts, codespacesbackend.WithMachine(codespacesSettings.Machine))
	}
	if codespacesSettings.IdleTimeout != "" {
		opts = append(opts, codespacesbackend.WithIdleTimeout(codespacesSettings.IdleTimeout))
	}
	if codespacesSettings.DevcontainerPath != "" {
		opts = append(opts, codespacesbackend.WithDevcontainerPath(codespacesSettings.DevcontainerPath))
	}

	return codespacesbackend.New(opts...)
}

// repoFromRemoteURL extracts "owner/repo" from a git remote URL.
// Supports both SSH (git@github.com:owner/repo.git) and HTTPS
// (https://github.com/owner/repo.git) formats.
func repoFromRemoteURL(remoteURL string) string {
	// SSH format: git@github.com:owner/repo.git
	if strings.Contains(remoteURL, ":") && strings.Contains(remoteURL, "@") {
		parts := strings.SplitN(remoteURL, ":", 2)
		if len(parts) == 2 {
			repo := strings.TrimSuffix(parts[1], ".git")
			return repo
		}
	}

	// HTTPS format: https://github.com/owner/repo.git
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	parts := strings.Split(remoteURL, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}

	return remoteURL
}

// homeDirForFleet looks up the named fleet's persisted HomeDir
// setting, returning empty when state can't be loaded or the fleet
// isn't recorded yet. Callers pass the result straight to
// fleetlaunch.EnsureFleetRC, which substitutes its DefaultHomeDir
// when given empty — so missing state silently falls back rather
// than failing the stage.
func homeDirForFleet(fleetName string) string {
	st, err := state.Load()
	if err != nil {
		return ""
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return ""
	}
	return f.Settings.HomeDir
}

func setFailed(fleetName, instanceName string, origErr error) {
	st, err := state.Load()
	if err != nil {
		return
	}
	if f, ok := st.Fleets[fleetName]; ok {
		if instance, err := f.GetInstance(instanceName); err == nil {
			instance.Status = fleet.StatusFailed
			instance.Error = origErr.Error()
		}
	}
	_ = state.Save(st)
}
