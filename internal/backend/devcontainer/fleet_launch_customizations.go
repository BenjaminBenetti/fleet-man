package devcontainer

// FleetLaunchCustomizations configures fleet-man's built-in Fleet
// Launch page.
type FleetLaunchCustomizations struct {
	// Sites is the list of links shown on the Fleet Launch page's Links
	// tab. An empty list means "no link entries configured".
	Sites []FleetLaunchSite `json:"sites"`
	// Apps is the list of embedded apps shown on the Fleet Launch page.
	// Each app becomes its own tab; opening the tab runs the app's
	// command and iframes its localhost port. An empty list means "no
	// app tabs".
	Apps []FleetLaunchApp `json:"apps"`
}

// Configured reports whether Fleet Launch has anything to show — at
// least one site link or one app tab. The browser launch logic uses this
// to decide whether the Fleet Launch page is a valid target.
func (fl FleetLaunchCustomizations) Configured() bool {
	return len(fl.Sites) > 0 || len(fl.Apps) > 0
}
