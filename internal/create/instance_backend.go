package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// newInstanceBackend constructs the backend a provisioning job acts through,
// bound to the record of the instance it provisions: the CREATING record for a
// create, the CLONING destination for a clone, the instance itself for a
// rebuild. It is a package var so tests can see which record each job binds
// and substitute a fake backend.
var newInstanceBackend = backendutil.NewForInstance

// createBackend returns the backend RunContext provisions rec through. Coder
// and Codespaces take create-time options (template, preset, parameters, repo,
// branch, machine type) that only a create needs, so they keep their own
// builders. Every other backend is bound to rec through newInstanceBackend.
func createBackend(rec *fleet.Instance, fleetName, remoteURL, branch string, verbose bool) backend.Backend {
	switch rec.Backend {
	case fleet.BackendCoder:
		return buildCoderBackend(fleetName, rec.Name, remoteURL, branch, verbose)
	case fleet.BackendCodespaces:
		return buildCodespacesBackend(remoteURL, branch, verbose)
	default:
		return newInstanceBackend(rec, verbose)
	}
}

// createRecord returns the record a create job binds its backend to: the
// CREATING record the server wrote before starting the job, which carries the
// instance's whole identity (its creation time and anything else a backend
// binds beyond its type). When that record can't be read (it was removed while
// the job was starting, or state is unreadable) the create proceeds as it
// always has, on a record built from the job's own arguments. Either way the
// Backend is the type the job was started with.
func createRecord(fleetName, instanceName, wsDir, branch string, backendType fleet.BackendType) *fleet.Instance {
	var rec *fleet.Instance
	if st, err := state.Load(); err == nil {
		rec = persistedRecord(st, fleetName, instanceName)
	}
	if rec == nil {
		rec = &fleet.Instance{
			Name:         instanceName,
			WorkspaceDir: wsDir,
			Status:       fleet.StatusCreating,
			Branch:       branch,
		}
	}
	rec.Backend = backendType
	return rec
}

// persistedRecord returns fleetName/instanceName's record from st, or nil when
// st has no such fleet or instance.
func persistedRecord(st *state.State, fleetName, instanceName string) *fleet.Instance {
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		return nil
	}
	return inst
}
