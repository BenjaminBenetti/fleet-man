package fleet

import (
	"fmt"
	"regexp"
	"strings"
)

// Per-fleet Coder settings (issue #221). Like custom mounts and layout
// presets, the user input is normalized at the domain layer so the TUI (for
// immediate UX feedback) and the server (authoritatively, in SetFleetSettings)
// share one definition of what is legal.

// coderWorkspaceNameRe matches a legal coder workspace-name override: coder
// workspace names are lowercase-insensitive alphanumerics and hyphens starting
// with an alphanumeric. The override is only the prefix of the final
// "<name>-<instance>" workspace name, so a trailing hyphen is rejected too.
var coderWorkspaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// coderWorkspaceNameMaxLen bounds the override so the composed
// "<name>-<instance>" still leaves room inside coder's 32-character
// workspace-name limit.
const coderWorkspaceNameMaxLen = 24

// NormalizeCoderSettings trims and validates a fleet's coder settings in
// place. Template and preset are free-form coder-side identifiers and are only
// trimmed; the workspace-name override becomes part of every workspace name
// this fleet creates, so it must be a legal coder name fragment (empty means
// "use the fleet name"). Parameter names are trimmed and empty-name entries
// dropped (a nameless binding can never be passed to `coder create`).
func NormalizeCoderSettings(s *FleetSettings) error {
	s.CoderTemplate = strings.TrimSpace(s.CoderTemplate)
	s.CoderPreset = strings.TrimSpace(s.CoderPreset)
	s.CoderWorkspaceName = strings.TrimSpace(s.CoderWorkspaceName)
	if s.CoderWorkspaceName != "" {
		if len(s.CoderWorkspaceName) > coderWorkspaceNameMaxLen {
			return fmt.Errorf("coder workspace name %q is too long (max %d characters)", s.CoderWorkspaceName, coderWorkspaceNameMaxLen)
		}
		if !coderWorkspaceNameRe.MatchString(s.CoderWorkspaceName) {
			return fmt.Errorf("coder workspace name %q must be alphanumerics and hyphens, starting and ending with an alphanumeric", s.CoderWorkspaceName)
		}
	}
	params := s.CoderParameters[:0]
	for _, p := range s.CoderParameters {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			continue
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		params = nil
	}
	s.CoderParameters = params
	return nil
}
