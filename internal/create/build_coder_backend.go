package create

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

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
