package fleet

import (
	"fmt"
	"strings"
	"unicode"
)

// ValidateInstanceName rejects names that cannot be embedded verbatim in the
// places an instance name ends up: container names, workspace paths, and tmux
// session prefixes. Every path that accepts a name — create, clone, and the
// display-name edit (rename) — calls this so the rule has a single source of
// truth (issue #143).
func ValidateInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("instance name must not be empty")
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("instance name %q must not contain spaces", name)
	}
	return nil
}
