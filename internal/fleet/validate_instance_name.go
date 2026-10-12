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
//
// An instance's checkout is ~/.fleet/workspaces/<fleet>/<name>/, beside the
// fleet's own folders there: the shared files and the agents' folders, the
// custom mounts (.mnt), host binds (.hostbind), named volumes (.vol) and the
// caches. So a name is one path segment, and neither "files" nor one that
// starts with a dot, which would put the checkout inside one of those (and,
// on a cluster, sync it into every pod of the fleet).
func ValidateInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("instance name must not be empty")
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("instance name %q must not contain spaces", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("instance name %q must not contain path separators", name)
	}
	if name == reservedFilesFolder || strings.HasPrefix(name, ".") {
		return fmt.Errorf("instance name %q is reserved for the fleet's own folders", name)
	}
	return nil
}

// reservedFilesFolder is the fleet's shared-files folder beside its
// instances' checkouts (mounts/resolver's sharedFilesSubdir).
const reservedFilesFolder = "files"
