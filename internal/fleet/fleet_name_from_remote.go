package fleet

import (
	"net/url"
	"path"
	"strings"
)

// FleetNameFromRemote extracts a fleet name from a git remote URL.
// Examples:
//
//	git@github.com:org/fleet-man.git → fleet-man
//	https://github.com/org/fleet-man.git → fleet-man
//	https://github.com/org/fleet-man → fleet-man
//
// A file:// template remote yields "" on purpose: a local directory name is
// not an authoritative project name, so the user must supply the fleet name
// explicitly (see TemplateNameHint for the default the TUI offers).
func FleetNameFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if IsTemplateRemote(remote) {
		return ""
	}

	// Handle SSH-style: git@github.com:org/repo.git
	if strings.Contains(remote, ":") && !strings.Contains(remote, "://") {
		parts := strings.SplitN(remote, ":", 2)
		if len(parts) == 2 {
			remote = parts[1]
		}
	} else {
		// Handle HTTPS-style URLs
		parsed, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		remote = parsed.Path
	}

	// Get the last path component and strip .git suffix
	name := path.Base(remote)
	name = strings.TrimSuffix(name, ".git")
	return name
}
