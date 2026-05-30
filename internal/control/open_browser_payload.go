package control

// OpenBrowserPayload is the body of a TypeOpenBrowser Envelope: the address
// the host browser should open or navigate to.
type OpenBrowserPayload struct {
	// URL is the address to open. The host opens its proxied browser to this
	// URL (or forwards it as a new tab to an already-running browser).
	URL string `json:"url"`
}
