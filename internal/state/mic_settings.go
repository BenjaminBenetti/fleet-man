package state

// MicSettings holds the virtual-microphone preferences: proxying the mic on the
// machine the TUI runs on into fleet instances, so voice input (e.g. Claude
// Code's voice mode) works inside a headless container.
type MicSettings struct {
	// Enabled gates the whole feature daemon-side: when true, new instances get
	// the audio packages + virtual-source config installed at provision time and
	// the daemon attaches a sink to every running instance while a microphone
	// provider (a TUI) is connected. When false no virtual device is injected
	// and nothing is installed. Defaults to false — a microphone is opt-in.
	Enabled bool `json:"enabled,omitempty"`

	// Device is the capture device on the CLIENT's machine, as an id from the
	// client's own enumeration (internal/mic). Empty means the system default.
	// A device that is not present on the capturing machine falls back to the
	// system default rather than failing, since one config may be shared by
	// clients on different machines.
	Device string `json:"device,omitempty"`
}
