package startup

import "github.com/BenjaminBenetti/fleet-man/internal/fleet"

// ScriptsFor returns the ordered list of startup scripts that match
// the given fleet settings. Each toggle in FleetSettings that has an
// associated install script contributes exactly one Script; toggles
// without an associated script are silently skipped.
//
// The order is fixed (Claude Code first, Codex second, Auggie third) so
// users get deterministic log filenames and stable run-ordering across
// instance creations within the same fleet.
func ScriptsFor(settings fleet.FleetSettings) []Script {
	var scripts []Script
	if settings.ClaudeCodeMount {
		scripts = append(scripts, claudeCodeScript())
	}
	if settings.CodexMount {
		scripts = append(scripts, codexScript())
	}
	if settings.AuggieMount {
		scripts = append(scripts, auggieScript())
	}
	return scripts
}
