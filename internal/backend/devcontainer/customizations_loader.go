package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// devcontainerConfig is the narrow view of a devcontainer.json that
// fleet-man decodes. Only the customizations block is modelled; every
// other top-level field is intentionally ignored so unrelated schema
// changes can never break this parse.
type devcontainerConfig struct {
	Customizations Customizations `json:"customizations"`
}

// LoadFleetCustomizations reads the devcontainer.json under workspaceDir
// and returns fleet-man's "customizations.fleet" block.
//
// A missing devcontainer.json, an absent customizations/fleet block, or
// any unset individual field all surface as the zero value rather than
// an error: callers treat the result as "defaults" and only override
// behaviour where a field is explicitly set. A genuine read or JSON
// parse failure is returned so callers can decide whether to surface or
// ignore it.
func LoadFleetCustomizations(workspaceDir string) (FleetCustomizations, error) {
	_, rawJSON, err := findDevcontainerJSON(workspaceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FleetCustomizations{}, nil
		}
		return FleetCustomizations{}, err
	}

	// devcontainer.json is JSONC (comments, trailing commas); reuse the
	// same strict-JSON conversion the mount-conflict logic relies on.
	var cfg devcontainerConfig
	if err := json.Unmarshal(toStrictJSON(rawJSON), &cfg); err != nil {
		return FleetCustomizations{}, fmt.Errorf("parse devcontainer.json customizations: %w", err)
	}
	return cfg.Customizations.Fleet, nil
}

// LoadFleetCustomizationsFromFile reads fleet-man's "customizations.fleet"
// block from an explicit devcontainer.json at path, for callers handed a
// concrete file (e.g. `fleet launch --config ./path/to/devcontainer.json`)
// rather than a workspace directory to search.
//
// Unlike LoadFleetCustomizations, a missing file IS an error here: the path
// was named explicitly, so its absence is a mistake the caller wants to hear
// about rather than silently treat as "defaults". An absent customizations or
// fleet block still surfaces as the zero value — only unset fields, not a
// missing file, mean "use defaults". The file is parsed through the same
// JSONC-tolerant toStrictJSON conversion as the workspace loader, so comments
// and trailing commas are accepted.
func LoadFleetCustomizationsFromFile(path string) (FleetCustomizations, error) {
	rawJSON, err := os.ReadFile(path)
	if err != nil {
		return FleetCustomizations{}, err
	}

	// devcontainer.json is JSONC (comments, trailing commas); reuse the
	// same strict-JSON conversion the workspace loader and mount-conflict
	// logic rely on.
	var cfg devcontainerConfig
	if err := json.Unmarshal(toStrictJSON(rawJSON), &cfg); err != nil {
		return FleetCustomizations{}, fmt.Errorf("parse devcontainer.json customizations: %w", err)
	}
	return cfg.Customizations.Fleet, nil
}
