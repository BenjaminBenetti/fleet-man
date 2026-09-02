package fleet

import (
	"fmt"
	"strings"
	"unicode"
)

// ValidateFleetName rejects names that cannot be embedded verbatim where a
// fleet name ends up: the ~/.fleet/workspaces/<fleet> directory, container
// names, and the fleet/instance target syntax. Template fleets are the first
// place a user TYPES a fleet name (git fleets derive theirs from the URL), so
// the rule lives here rather than in the dialog.
func ValidateFleetName(name string) error {
	if name == "" {
		return fmt.Errorf("fleet name must not be empty")
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("fleet name %q must not contain spaces", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("fleet name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("fleet name %q is not allowed", name)
	}
	return nil
}
