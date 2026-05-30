package fleet

import "fmt"

// FleetNameFromCwd reads the git remote origin URL from the current directory
// and derives a fleet name from it.
func FleetNameFromCwd() (string, error) {
	remote, err := RemoteURLFromCwd()
	if err != nil {
		return "", err
	}
	name := FleetNameFromRemote(remote)
	if name == "" {
		return "", fmt.Errorf("could not parse fleet name from remote %q", remote)
	}
	return name, nil
}
