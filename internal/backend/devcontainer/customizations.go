package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ===========================================
// Customization types
// ===========================================
//
// These types model fleet-man's slice of a devcontainer.json. The
// devcontainer spec lets each tool carve out a namespaced sub-object
// under the top-level "customizations" key (VS Code uses "vscode",
// GitHub uses "codespaces", Coder uses "coder", and so on); fleet-man
// reads its own "fleet" namespace and ignores every other tool's block.
//
//	{
//	  "customizations": {
//	    "fleet": {
//	      "browser": { "initialUrl": "http://localhost:3000" }
//	    }
//	  }
//	}
//
// The whole "fleet" block is decoded into FleetCustomizations rather
// than reaching in for individual keys, so adding a new project-level
// setting is just a new field here — no new parsing code, and the
// on-disk schema and in-memory shape stay in lockstep.

// Customizations mirrors the top-level "customizations" object in a
// devcontainer.json. Only the "fleet" namespace is modelled; sibling
// tool namespaces are left unparsed.
type Customizations struct {
	Fleet FleetCustomizations `json:"fleet"`
}

// FleetCustomizations is fleet-man's namespace inside a devcontainer.json
// "customizations" block — the single typed home for every project-level
// setting fleet-man understands. New settings are added as new fields.
type FleetCustomizations struct {
	// Browser configures the built-in browser launch feature.
	Browser BrowserCustomizations `json:"browser"`
}

// BrowserCustomizations configures fleet-man's built-in browser launch.
type BrowserCustomizations struct {
	// InitialURL is the address the browser opens to instead of
	// about:blank. An empty value means "use fleet-man's default".
	InitialURL string `json:"initialUrl"`

	// LandingPage configures fleet-man's built-in landing page — a
	// directory of links to the dev services running inside the
	// instance.
	LandingPage LandingPageCustomizations `json:"landingPage"`
}

// LandingPageCustomizations configures fleet-man's built-in browser
// landing page.
type LandingPageCustomizations struct {
	// Sites is the list of links shown on the landing page. An empty
	// list means "no landing page entries configured".
	Sites []LandingPageSite `json:"sites"`
}

// LandingPageSite is a single entry on the browser landing page.
type LandingPageSite struct {
	// Title is the primary label shown for the link.
	Title string `json:"title"`
	// SubTitle is the secondary descriptive text shown under the title.
	SubTitle string `json:"subTitle"`
	// URL is the address the link navigates to.
	URL string `json:"url"`
	// HealthCheck is an address polled to indicate whether the service
	// is reachable. An empty value means "no health check".
	HealthCheck string `json:"healthCheck"`
}

// ===========================================
// Loading
// ===========================================

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
	_, raw, err := findDevcontainerJSON(workspaceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FleetCustomizations{}, nil
		}
		return FleetCustomizations{}, err
	}

	// devcontainer.json is JSONC (comments, trailing commas); reuse the
	// same strict-JSON conversion the mount-conflict logic relies on.
	var cfg devcontainerConfig
	if err := json.Unmarshal(toStrictJSON(raw), &cfg); err != nil {
		return FleetCustomizations{}, fmt.Errorf("parse devcontainer.json customizations: %w", err)
	}
	return cfg.Customizations.Fleet, nil
}
