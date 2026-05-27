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
	// Sites is the list of links shown on the landing page's Links tab. An
	// empty list means "no link entries configured".
	Sites []LandingPageSite `json:"sites"`
	// Apps is the list of embedded apps shown on the landing page. Each app
	// becomes its own tab; opening the tab runs the app's command and
	// iframes its localhost port. An empty list means "no app tabs".
	Apps []LandingPageApp `json:"apps"`
}

// Configured reports whether the landing page has anything to show — at
// least one site link or one app tab. The browser launch logic uses this
// to decide whether the Fleet Launch page is a valid target.
func (lp LandingPageCustomizations) Configured() bool {
	return len(lp.Sites) > 0 || len(lp.Apps) > 0
}

// LandingPageApp is a single embedded app shown as a tab on the landing
// page. Opening the tab starts the app (if its port isn't already
// answering) and iframes http://localhost:<port>.
type LandingPageApp struct {
	// Title is the label shown on the app's tab.
	Title string `json:"title"`
	// Command is a bash command that starts the app. It is run the first
	// time the tab is opened, unless the app's port is already reachable
	// (so a second open or a browser relaunch doesn't double-start it). An
	// empty command means "the app is already running; just iframe it".
	Command string `json:"command"`
	// Port is the localhost port the app serves on. The tab iframes
	// http://localhost:<port> once it answers.
	Port int `json:"port"`
}

// LandingPageSite is a single entry on the browser landing page.
type LandingPageSite struct {
	// Title is the primary label shown for the link.
	Title string `json:"title"`
	// Icon is an optional image shown before the title. Its value is used
	// verbatim as an <img> src, so any path the browser can load works
	// (e.g. an https URL or a data URI). An empty value means "no icon".
	Icon string `json:"icon"`
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
