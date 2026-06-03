// Package configutil is the pure-read carve-out for config.json. It lets
// pre-server client code — the cli root.go tmux-keybind read and `fleet shell`'s
// shell-command build, both of which run BEFORE (or without) a fleet server —
// read config without importing internal/state directly, which the depguard
// client boundary forbids. This is the carve-out documented in config.proto: a
// pure local read, NOT an RPC, because of the chicken-and-egg (the config is
// read to build the tmux session that launches the TUI that starts the server).
//
// It is deliberately thin: it does not duplicate the Config types, it just
// re-exposes the loader. configutil is not itself client code, so it may import
// internal/state.
package configutil

import "github.com/BenjaminBenetti/fleet-man/internal/state"

// LoadConfig reads ~/.fleet/config.json, returning defaults when absent.
func LoadConfig() (*state.Config, error) {
	return state.LoadConfig()
}
