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

// DefaultConfig returns a config with defaults applied.
func DefaultConfig() *Config { return state.DefaultConfig() }

// The persisted/config model TYPES are re-exported as aliases so client code
// (internal/tui) can name them without importing internal/state directly — the
// depguard boundary forbids that import, but the boundary's intent is to stop
// clients from ACCESSING state/backends (Load/Save/exec), which they no longer
// do (they go through the server's RPCs). The struct shapes are just DTOs the
// client fills from the server's snapshot. (A later cleanup can render straight
// from the fleetgrpc proto types and retire these.)
type (
	Config             = state.Config
	GeneralSettings    = state.GeneralSettings
	AgentSettings      = state.AgentSettings
	DotfilesSettings   = state.DotfilesSettings
	CodespacesSettings = state.CodespacesSettings
	BrowserSettings    = state.BrowserSettings
	AgentTool          = state.AgentTool
	State              = state.State
	GroupLayout        = state.GroupLayout
	ArmadaRemote       = state.ArmadaRemote
)

// AgentTool values, re-exported alongside the type alias.
const (
	AgentToolCodex   = state.AgentToolCodex
	AgentToolClaude  = state.AgentToolClaude
	AgentToolGemini  = state.AgentToolGemini
	AgentToolCopilot = state.AgentToolCopilot
	AgentToolAuggie  = state.AgentToolAuggie
)
