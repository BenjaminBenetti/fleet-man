package svcruntime

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// BuildkitStep is when ConfigureBuildkit runs, which decides what it does.
type BuildkitStep int

const (
	// BuildkitCreate is a create, clone or rebuild: the instance's shared
	// builder is made again and becomes its default.
	BuildkitCreate BuildkitStep = iota
	// BuildkitStart is a start: an instance without the shared builder
	// gets it; one with it keeps it as it is.
	BuildkitStart
	// BuildkitRenew is the daemon's renewal of the instance's credential:
	// only the credential.
	BuildkitRenew
)

// ConfigureBuildkit points instanceName's docker buildx at fleetName's shared
// Buildkit server on rt, the instance's runtime, through b, its backend (in
// workspaceDir). Where rt authenticates instances (CredentialIssuer), it
// first issues the instance a credential of its own, which goes into the
// instance (buildkit.InstallCredential) whatever tools it has, and is
// recorded installed. The builder needs docker and buildx in the instance:
// without them it is skipped, and configured is false.
//
// An error wrapping ErrNoEndpoint is a runtime that does not point the
// fleet's instances at its server, saying why; nothing was done. Any error is
// for the caller to warn about: the instance works without the shared cache.
func ConfigureBuildkit(rt Runtime, b backend.Backend, fleetName, instanceName, workspaceDir string, step BuildkitStep) (configured bool, err error) {
	ep, err := rt.Endpoint(fleetName, fleet.ServiceBuildkit)
	if err != nil {
		return false, err
	}
	builder := buildkit.SocketBuilder()
	if ep.URL != "" {
		builder = buildkit.Builder{Endpoint: ep.URL}
	}
	if issuer, ok := rt.(CredentialIssuer); ok {
		cred, err := issuer.IssueCredential(fleetName, instanceName, fleet.ServiceBuildkit)
		if err == nil {
			err = buildkit.InstallCredential(b, workspaceDir, cred)
		}
		if err != nil {
			if step == BuildkitCreate {
				// A clone has its source's credential: without one of
				// its own it must not keep that.
				if rerr := buildkit.RemoveCredential(b, workspaceDir); rerr != nil {
					flog.Warn("buildkit: removing a credential the instance has no claim to", "fleet", fleetName, "instance", instanceName, "err", rerr)
				}
			}
			return false, err
		}
		if err := issuer.CredentialInstalled(fleetName, instanceName, fleet.ServiceBuildkit, cred); err != nil {
			// The instance has it; only the renewal's record of it is
			// behind, which renews it again sooner.
			flog.Warn("buildkit: recording an installed credential", "fleet", fleetName, "instance", instanceName, "err", err)
		}
		builder.ServerName = cred.ServerName
	}
	switch step {
	case BuildkitRenew:
		return false, nil
	case BuildkitStart:
		return buildkit.EnsureInstanceBuildx(b, workspaceDir, builder)
	}
	return buildkit.ConfigureInstanceBuildx(b, workspaceDir, builder)
}
