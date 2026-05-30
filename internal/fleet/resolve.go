package fleet

import (
	"fmt"
	"strings"
)

// Target represents a resolved fleet + instance name pair.
type Target struct {
	Fleet    string
	Instance string
}

// Resolve parses a user-provided name into a fleet/instance target.
// It handles three forms:
//   - "instance"           → fleet inferred from cwd git remote
//   - "fleet/instance"     → explicit fleet name
//   - "instance" + repoURL → fleet derived from the repo URL
func Resolve(name string, repoFlag string) (*Target, error) {
	if repoFlag != "" {
		fleetName := FleetNameFromRemote(repoFlag)
		if fleetName == "" {
			return nil, fmt.Errorf("could not derive fleet name from repo URL %q", repoFlag)
		}
		return &Target{Fleet: fleetName, Instance: name}, nil
	}

	if strings.Contains(name, "/") {
		parts := strings.SplitN(name, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid target %q: both fleet and instance names are required", name)
		}
		return &Target{Fleet: parts[0], Instance: parts[1]}, nil
	}

	// Infer fleet from cwd git remote
	fleetName, err := FleetNameFromCwd()
	if err != nil {
		return nil, fmt.Errorf("could not infer fleet from cwd: %w (use fleet/instance or --repo)", err)
	}
	return &Target{Fleet: fleetName, Instance: name}, nil
}
