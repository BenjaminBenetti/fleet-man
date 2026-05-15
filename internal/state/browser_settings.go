package state

// BrowserSettings holds user preferences for the in-fleet browser proxy
// feature. Browser profile state (bookmarks, passwords, cookies) is
// persisted under ~/.fleet/workspaces/<fleet>/ so it survives instance
// churn; this setting controls whether all instances of a fleet share a
// single profile or each instance gets its own.
type BrowserSettings struct {
	// MultipleBrowsersPerFleet controls the profile layout:
	//   nil/false → one shared profile at <fleet>/.browser
	//   true      → per-instance profile at <fleet>/<instance>/.browser
	// Per-fleet (default) gives bookmark/password continuity across
	// instances. Per-instance lets two browsers run at once for the
	// same fleet at the cost of duplicated profile state.
	MultipleBrowsersPerFleet *bool `json:"multiple_browsers_per_fleet,omitempty"`
}

// MultipleBrowsersPerFleetEnabled reports whether each instance should
// get its own browser data directory. Defaults to false.
func (b BrowserSettings) MultipleBrowsersPerFleetEnabled() bool {
	if b.MultipleBrowsersPerFleet == nil {
		return false
	}
	return *b.MultipleBrowsersPerFleet
}
