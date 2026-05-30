package gitutil

import (
	"os/exec"
	"strings"
)

// BranchName returns the checked-out branch for a workspace, or "" when the
// workspace is not on a named branch.
func BranchName(workspaceDir string) string {
	if workspaceDir == "" {
		return ""
	}

	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}

	output, err := exec.Command("git", "-C", workspaceDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}

	branch := strings.TrimSpace(string(output))
	if branch == "" || branch == "HEAD" {
		return ""
	}

	return branch
}
