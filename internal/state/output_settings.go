package state

// OutputSettings selects the client that plays instance audio. An empty Client
// is Auto (the newest attached playback client); an empty Device is its system
// default. Output defaults to on and is independent of microphone capture.
type OutputSettings struct {
	// LoadConfig seeds missing settings from DefaultConfig. Always persist
	// false so an explicitly disabled output survives a save and reload.
	Enabled bool   `json:"enabled"`
	Client  string `json:"client,omitempty"`
	Device  string `json:"device,omitempty"`
}
