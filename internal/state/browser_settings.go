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

	// AutoSwitch silences the "another browser is running, switch?"
	// prompt and kills+relaunches automatically. Only meaningful when
	// MultipleBrowsersPerFleet is false — in per-instance mode each
	// instance owns its own data dir, so the prompt has a different
	// meaning (restart this instance's browser) and isn't auto-suppressed.
	AutoSwitch *bool `json:"auto_switch,omitempty"`
}

// MultipleBrowsersPerFleetEnabled reports whether each instance should
// get its own browser data directory. Defaults to false.
func (b BrowserSettings) MultipleBrowsersPerFleetEnabled() bool {
	if b.MultipleBrowsersPerFleet == nil {
		return false
	}
	return *b.MultipleBrowsersPerFleet
}

// AutoSwitchEnabled reports whether the browser-switch confirmation
// dialog should be skipped. Defaults to false. Only consulted in
// per-fleet (shared profile) mode.
func (b BrowserSettings) AutoSwitchEnabled() bool {
	if b.AutoSwitch == nil {
		return false
	}
	return *b.AutoSwitch
}
