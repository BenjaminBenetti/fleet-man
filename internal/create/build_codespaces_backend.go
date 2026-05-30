package create

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

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
