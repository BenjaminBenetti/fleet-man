package devcontainer

// BrowserCustomizations configures fleet-man's built-in browser launch.
type BrowserCustomizations struct {
	// InitialURL is the address the browser opens to instead of
	// about:blank. An empty value means "use fleet-man's default".
	InitialURL string `json:"initialUrl"`
}
