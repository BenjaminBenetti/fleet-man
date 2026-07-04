package backendutil

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	devcontainerbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// New creates a Backend for the given type.
func New(backendType fleet.BackendType, verbose bool) backend.Backend {
	switch backendType {
	case fleet.BackendCoder:
		opts := []coderbackend.Option{}
		if verbose {
			opts = append(opts, coderbackend.WithVerbose(verbose))
		}
		return coderbackend.New(opts...)
	case fleet.BackendCodespaces:
		opts := []codespacesbackend.Option{}
		if verbose {
			opts = append(opts, codespacesbackend.WithVerbose(verbose))
		}
		return codespacesbackend.New(opts...)
	default:
		opts := []devcontainerbackend.Option{}
		if verbose {
			opts = append(opts, devcontainerbackend.WithVerbose(verbose))
		}
		return devcontainerbackend.New(opts...)
	}
}

// NewForInstance creates a Backend for the given instance, pre-registering
// the codespace/coder name mapping when applicable so that Exec/ExecCommand
// use the correct container ID.
func NewForInstance(instance *fleet.Instance, verbose bool) backend.Backend {
	instanceBackend := New(instance.Backend, verbose)
	if instance.ContainerID == "" {
		return instanceBackend
	}
	switch instance.Backend {
	case fleet.BackendCodespaces:
		if codespacesBackend, ok := instanceBackend.(*codespacesbackend.CodespacesBackend); ok {
			codespacesBackend.RegisterName(instance.WorkspaceDir, instance.ContainerID)
		}
	case fleet.BackendCoder:
		// Coder workspace names are per-fleet configurable (issue #221), so
		// path-based methods must target the recorded workspace, not a
		// path-derived name.
		if coderBackend, ok := instanceBackend.(*coderbackend.CoderBackend); ok {
			coderBackend.RegisterName(instance.WorkspaceDir, instance.ContainerID)
		}
	}
	return instanceBackend
}

// CoderOpenVSCodeArgs builds args for `coder open vscode`.
// containerID may be "workspace" or "workspace.agent" — both forms
// are accepted directly by the coder CLI.
func CoderOpenVSCodeArgs(containerID string) []string {
	return []string{"open", "vscode", containerID}
}
