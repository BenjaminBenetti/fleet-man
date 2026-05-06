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
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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
func Run(fleetName, instanceName, remoteURL, branch string, verbose bool, bt fleet.BackendType) error {
	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, instanceName, fleetName)

	var dc backend.Backend
	switch bt {
	case fleet.BackendCoder:
		dc = buildCoderBackend(fleetName, instanceName, remoteURL, branch, verbose)
	case fleet.BackendCodespaces:
		dc = buildCodespacesBackend(remoteURL, branch, verbose)
	default:
		dc = backendutil.New(bt, verbose)
	}

	// Backends that operate on a host-side workspace dir need the repo
	// cloned locally before Up() runs. Coder and Codespaces clone inside
	// their remote workspace via their own template/devcontainer flow.
	if bt == fleet.BackendDevcontainer || bt == fleet.BackendLocalDir || bt == "" {
		// Devcontainer / local dir: clone repo first, then provision
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

	result, err := dc.Up(wsDir)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Auto-install dotfiles. A failure here is non-fatal — the instance
	// is still usable, so we mark it running and surface the error as a
	// warning rather than blocking the whole creation.
	cfg, _ := state.LoadConfig()
	if cfg != nil && cfg.DotfilesSettings.AutoInstall {
		if script := dotfiles.SetupScript(cfg); script != "" {
			cmd := dc.ExecCommand(wsDir, []string{"sh", "-c", script})
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
		if inst, err := f.GetInstance(instanceName); err == nil {
			inst.ContainerID = result.ContainerID
			inst.Status = fleet.StatusRunning
			inst.Error = ""
		}
	}
	return state.Save(st)
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

	cfg, err := state.LoadConfig()
	if err != nil || cfg == nil {
		return coderbackend.New(opts...)
	}

	cs := cfg.CoderSettings
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
			val := p.Value
			if val == "" {
				val = p.DefaultValue
			}
			val = strings.ReplaceAll(val, "${GIT_URL}", remoteURL)
			val = strings.ReplaceAll(val, "${GIT_BRANCH}", branch)
			val = strings.ReplaceAll(val, "${INSTANCE_NAME}", wsName)
			if val != "" {
				resolved[p.Name] = val
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

	cfg, err := state.LoadConfig()
	if err != nil || cfg == nil {
		return codespacesbackend.New(opts...)
	}

	cs := cfg.CodespacesSettings
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
		if inst, err := f.GetInstance(instanceName); err == nil {
			inst.Status = fleet.StatusFailed
			inst.Error = origErr.Error()
		}
	}
	_ = state.Save(st)
}
