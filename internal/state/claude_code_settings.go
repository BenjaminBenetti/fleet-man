package state

// ClaudeCodeSettings holds the global Claude Code integration preferences.
type ClaudeCodeSettings struct {
	// StatusMod enables the fleet status mod: a Claude Code plugin, installed
	// into every instance's Claude Code, that draws a band above the prompt
	// with this instance's name and the agents at work across every fleet.
	StatusMod *bool `json:"status_mod,omitempty"` // nil = true (default on)
}

// StatusModEnabled reports whether the fleet status mod is on.
func (c ClaudeCodeSettings) StatusModEnabled() bool {
	if c.StatusMod == nil {
		return true
	}
	return *c.StatusMod
}
