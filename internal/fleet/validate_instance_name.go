package fleet

import (
	"fmt"
	"strings"
	"unicode"
)

// ValidateInstanceName rejects names that cannot be embedded verbatim in the
// places an instance name ends up: container names, workspace paths, and tmux
// session prefixes. Every path that accepts a name — create and clone —
// calls this so the rule has a single source of truth (issue #143); the
// display-name edit (rename) takes its first part, ValidateDisplayName.
//
// An instance's checkout is ~/.fleet/workspaces/<fleet>/<name>/, beside the
// fleet's own folders there: the shared files and the agents' folders, the
// custom mounts (.mnt), host binds (.hostbind), named volumes (.vol) and the
// caches. So a name is one path segment, and neither "files" nor one that
// starts with a dot, which would put the checkout inside one of those (and,
// on a cluster, sync it into every pod of the fleet).
func ValidateInstanceName(name string) error {
	if err := ValidateDisplayName(name); err != nil {
		return err
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("instance name %q must not contain path separators", name)
	}
	if name == reservedFilesFolder || strings.HasPrefix(name, ".") {
		return fmt.Errorf("instance name %q is reserved for the fleet's own folders", name)
	}
	return nil
}

// ValidateDisplayName is the rule for an instance's display name (the
// rename path, and a clone's new display name): not empty and without
// spaces, as for a name. A display name is a label only, never part of a
// path, so the folder rules of ValidateInstanceName do not apply to it.
func ValidateDisplayName(name string) error {
	if name == "" {
		return fmt.Errorf("instance name must not be empty")
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("instance name %q must not contain spaces", name)
	}
	return nil
}

// reservedFilesFolder is the fleet's shared-files folder beside its
// instances' checkouts (mounts/resolver's sharedFilesSubdir).
const reservedFilesFolder = "files"
