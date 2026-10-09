package tui

import "github.com/BenjaminBenetti/fleet-man/internal/configutil"

// placeholderConfig is used until the daemon's configuration is available.
// Output must stay off here: rendering or saving an unrelated setting after a
// failed load must not enable playback based on a fresh profile's default.
func placeholderConfig() *configutil.Config {
	cfg := configutil.DefaultConfig()
	cfg.OutputSettings.Enabled = false
	return cfg
}
