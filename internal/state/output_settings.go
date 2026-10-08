package state

// OutputSettings selects the client that plays instance audio. An empty Client
// is Auto (the newest attached playback client); an empty Device is its system
// default. Output is opt-in and independent of microphone capture.
type OutputSettings struct {
	Enabled bool   `json:"enabled"`
	Client  string `json:"client,omitempty"`
	Device  string `json:"device,omitempty"`
}
