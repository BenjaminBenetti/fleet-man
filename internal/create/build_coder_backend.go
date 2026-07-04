package create

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// buildCoderBackend creates a CoderBackend configured from the fleet's
// persisted settings (issue #221 — coder template/preset/parameters and the
// workspace-name override are per-fleet, no longer global config.json) with
// template, preset, workspace name, and resolved parameter bindings. The
// branch is exposed to template parameters via the ${GIT_BRANCH} substitution
// so Coder templates can clone the requested ref.
func buildCoderBackend(fleetName, instanceName, remoteURL, branch string, verbose bool) backend.Backend {
	opts := []coderbackend.Option{}
	if verbose {
		opts = append(opts, coderbackend.WithVerbose(true))
	}

	settings := coderFleetSettings(fleetName)

	// Workspace name: "<override-or-fleet>-<instance>", sanitized. Passed
	// explicitly because with an override in play the backend can no longer
	// re-derive the name from the workspace dir path.
	prefix := settings.CoderWorkspaceName
	if prefix == "" {
		prefix = fleetName
	}
	wsName := coderbackend.WorkspaceNameFor(prefix, instanceName)
	opts = append(opts, coderbackend.WithWorkspaceName(wsName))

	if settings.CoderTemplate != "" {
		opts = append(opts, coderbackend.WithTemplate(settings.CoderTemplate))
	}
	if settings.CoderPreset != "" {
		opts = append(opts, coderbackend.WithPreset(settings.CoderPreset))
	}

	// Resolve parameters with variable substitution
	if len(settings.CoderParameters) > 0 {
		resolved := make(map[string]string, len(settings.CoderParameters))
		for _, param := range settings.CoderParameters {
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

// coderFleetSettings looks up the named fleet's persisted settings, returning
// the zero value when state can't be loaded or the fleet isn't recorded —
// the workspace then gets the default "<fleet>-<instance>" name and no
// template/preset/parameters, matching the pre-settings behavior.
func coderFleetSettings(fleetName string) fleet.FleetSettings {
	st, err := state.Load()
	if err != nil {
		return fleet.FleetSettings{}
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return fleet.FleetSettings{}
	}
	return f.Settings
}
