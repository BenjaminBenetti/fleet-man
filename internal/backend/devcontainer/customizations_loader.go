package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// devcontainerConfigProbe decodes just enough of a devcontainer.json to
// tell whether the customizations block CONTAINS a "fleet" namespace:
// the pointer stays nil when the key is absent (or null), and points at
// the decoded block when present — even an empty {} one. The plain
// devcontainerConfig cannot make that distinction because its struct
// field decodes an absent key and an empty block to the same zero value.
type devcontainerConfigProbe struct {
	Customizations struct {
		Fleet *FleetCustomizations `json:"fleet"`
	} `json:"customizations"`
}

// LoadFleetCustomizationsUpward searches startDir and then each of its
// parents, up to the filesystem root, for a devcontainer.json whose
// customizations block contains a "fleet" namespace, and returns that
// block along with the path of the file it came from. It backs `fleet
// launch` run from anywhere inside a project tree, not just its root.
//
// A devcontainer.json WITHOUT a fleet block does not stop the climb:
// the project the user cares about may be a parent of an unconfigured
// nested one, and a file with no fleet block has nothing to offer
// fleet-man anyway. Reaching the root without a hit returns the zero
// value and an empty path — like LoadFleetCustomizations, absence means
// "defaults", not an error. A devcontainer.json that fails to parse IS
// an error (naming the file): silently climbing past a malformed config
// onto some ancestor's would launch the wrong project's links.
func LoadFleetCustomizationsUpward(startDir string) (FleetCustomizations, string, error) {
	// Absolutise so the parent walk terminates: Dir(".") is "." forever,
	// while Dir of an absolute path converges on the root.
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return FleetCustomizations{}, "", err
	}

	for {
		configPath, rawJSON, err := findDevcontainerJSON(dir)
		switch {
		case err == nil:
			var probe devcontainerConfigProbe
			if err := json.Unmarshal(toStrictJSON(rawJSON), &probe); err != nil {
				return FleetCustomizations{}, "", fmt.Errorf("parse %s customizations: %w", configPath, err)
			}
			if probe.Customizations.Fleet != nil {
				return *probe.Customizations.Fleet, configPath, nil
			}
		case !errors.Is(err, os.ErrNotExist):
			return FleetCustomizations{}, "", err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return FleetCustomizations{}, "", nil
		}
		dir = parent
	}
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
