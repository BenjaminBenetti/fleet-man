package backendutil

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	devcontainerbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	localdirbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/localdir"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// New creates a Backend for the given type.
func New(bt fleet.BackendType, verbose bool) backend.Backend {
	switch bt {
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
	case fleet.BackendLocalDir:
		opts := []localdirbackend.Option{}
		if verbose {
			opts = append(opts, localdirbackend.WithVerbose(verbose))
		}
		return localdirbackend.New(opts...)
	default:
		opts := []devcontainerbackend.Option{}
		if verbose {
			opts = append(opts, devcontainerbackend.WithVerbose(verbose))
		}
		return devcontainerbackend.New(opts...)
	}
}

// NewForInstance creates a Backend for the given instance, pre-registering
// the codespace name mapping when applicable so that Exec/ExecCommand
// use the correct container ID.
func NewForInstance(inst *fleet.Instance, verbose bool) backend.Backend {
	b := New(inst.Backend, verbose)
	if inst.Backend == fleet.BackendCodespaces && inst.ContainerID != "" {
		if csb, ok := b.(*codespacesbackend.CodespacesBackend); ok {
			csb.RegisterName(inst.WorkspaceDir, inst.ContainerID)
		}
	}
	return b
}

// CoderOpenVSCodeArgs builds args for `coder open vscode`.
// containerID may be "workspace" or "workspace.agent" — both forms
// are accepted directly by the coder CLI.
func CoderOpenVSCodeArgs(containerID string) []string {
	return []string{"open", "vscode", containerID}
}
