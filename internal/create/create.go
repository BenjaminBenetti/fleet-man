package create

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
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

	mounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	result, err := instanceBackend.Up(wsDir, mounts)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
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
				warning := fmt.Sprintf("dotfiles install failed: %v\n%s", err, detail)
				// Write warning to a file the TUI can pick up.
				warnPath := filepath.Join(state.FleetDir(), "logs", fleetName+"-"+instanceName+".warn")
				_ = os.WriteFile(warnPath, []byte(warning), 0644)
			}
		}
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
// translates them into a slice of backend.Mount values. Returns nil
// when the backend does not advertise SupportsCustomMounts so callers
// avoid wasted host-side preparation for cloud-managed backends.
//
// State load failures are tolerated: if state.json cannot be read the
// instance is still allowed to provision, just without any custom
// mounts. (The instance record we are about to fill in is loaded from
// the same file in the success path immediately after, which will
// surface a hard error then if the file is truly broken.)
func resolveCustomMounts(instanceBackend backend.Backend, fleetName string) ([]backend.Mount, error) {
	if !instanceBackend.SupportsCustomMounts() {
		return nil, nil
	}
	st, err := state.Load()
	if err != nil {
		return nil, nil
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, nil
	}
	return mountresolver.Resolve(fleetName, f.Settings)
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

	cs := config.CoderSettings
	if cs.Template != "" {
		opts = append(opts, coderbackend.WithTemplate(cs.Template))
	}
	if cs.Preset != "" {
		opts = append(opts, coderbackend.WithPreset(cs.Preset))
	}

	// Resolve parameters with variable substitution
	if len(cs.Parameters) > 0 {
		wsName := fleetName + "-" + instanceName
		resolved := make(map[string]string, len(cs.Parameters))
		for _, p := range cs.Parameters {
			value := p.Value
			if value == "" {
				value = p.DefaultValue
			}
			value = strings.ReplaceAll(value, "${GIT_URL}", remoteURL)
			value = strings.ReplaceAll(value, "${GIT_BRANCH}", branch)
			value = strings.ReplaceAll(value, "${INSTANCE_NAME}", wsName)
			if value != "" {
				resolved[p.Name] = value
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

	cs := config.CodespacesSettings
	if cs.Machine != "" {
		opts = append(opts, codespacesbackend.WithMachine(cs.Machine))
	}
	if cs.IdleTimeout != "" {
		opts = append(opts, codespacesbackend.WithIdleTimeout(cs.IdleTimeout))
	}
	if cs.DevcontainerPath != "" {
		opts = append(opts, codespacesbackend.WithDevcontainerPath(cs.DevcontainerPath))
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
