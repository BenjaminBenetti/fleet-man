package backendutil

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	devcontainerbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// newForType creates a Backend for the given type, bound to no instance. It is
// unexported so every construction site has to name the instance it acts on
// (NewForInstance) or asks about (CapabilitiesFor): a backend whose handle needs
// more of an instance's identity than its type then binds it here, in one place,
// instead of at each call site.
func newForType(backendType fleet.BackendType, verbose bool) backend.Backend {
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

// NewForInstance creates the Backend that acts on instance, pre-registering
// the codespace/coder name mapping when applicable so that Exec/ExecCommand
// use the correct container ID. It is the one constructor for a backend that
// does anything to an instance (create, clone dest, rebuild, start/stop, down,
// exec, pollers); an empty Backend type means devcontainer.
func NewForInstance(instance *fleet.Instance, verbose bool) backend.Backend {
	instanceBackend := newForType(instance.Backend, verbose)
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

// CapabilitiesFor returns a capability-only handle for instance's backend, for
// callers that only ask what the backend supports. It names the instance (not
// just its type) so a backend whose answers depend on where the instance runs
// can bind that, but it registers no names and does no I/O, so it is safe under
// the state lock. Use NewForInstance to act on the instance.
func CapabilitiesFor(instance *fleet.Instance) backend.Capabilities {
	return newForType(instance.Backend, false)
}

// CoderOpenVSCodeArgs builds args for `coder open vscode`.
// containerID may be "workspace" or "workspace.agent" — both forms
// are accepted directly by the coder CLI.
func CoderOpenVSCodeArgs(containerID string) []string {
	return []string{"open", "vscode", containerID}
}
