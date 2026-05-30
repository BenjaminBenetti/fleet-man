package devcontainer

// FleetLaunchApp is a single embedded app shown as a tab on the Fleet
// Launch page. Opening the tab starts the app (if its port isn't already
// answering) and iframes http://localhost:<port>.
type FleetLaunchApp struct {
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
