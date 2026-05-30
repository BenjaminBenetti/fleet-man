package devcontainer

// FleetCustomizations is fleet-man's namespace inside a devcontainer.json
// "customizations" block — the single typed home for every project-level
// setting fleet-man understands. New settings are added as new fields.
type FleetCustomizations struct {
	// Browser configures the built-in browser launch feature.
	Browser BrowserCustomizations `json:"browser"`

	// FleetLaunch configures fleet-man's built-in Fleet Launch page —
	// a directory of links and embedded apps for the dev services
	// running inside the instance. Sits at the same level as Browser
	// because it is its own feature; the browser merely opens to it.
	FleetLaunch FleetLaunchCustomizations `json:"fleetLaunch"`
}
