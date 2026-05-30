package fleet

import (
	"fmt"
	"os/exec"
	"strings"
)

// RemoteURLFromCwd reads the git remote origin URL from the current directory.
func RemoteURLFromCwd() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repo or no origin remote: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
