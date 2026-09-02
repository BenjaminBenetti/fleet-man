package create

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// copyTemplateTree materialises a template fleet's workspace: the directory a
// file:// remote points at is copied (cp -a, so permissions, mtimes, symlinks
// and any .git directory survive) into wsDir. It is the template counterpart of
// `git clone <remote> <wsDir>` — every new instance starts from a pristine copy
// of the template, and edits inside an instance never touch the template.
//
// Like git clone, it refuses a non-empty destination: a stale workspace dir (a
// destroy whose RemoveAll only warned, a hand-edited state.json) must surface
// as an error, not be silently merged under the template.
func copyTemplateTree(remoteURL, wsDir string) error {
	dir, err := fleet.ResolveTemplateDir(remoteURL)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(wsDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("workspace dir %s already exists and is not empty", wsDir)
	}
	if err := copyWorkspaceTree(dir, wsDir); err != nil {
		return fmt.Errorf("copy template %s: %w", dir, err)
	}
	return nil
}

// templateGitFileWarning reports a copied `.git` that is a FILE rather than a
// directory — a git worktree checkout or a submodule — whose gitdir pointer
// still targets the template's repository. Commits made inside the instance
// would then mutate the template's git state, so the user gets a warning.
// Returns "" when there is nothing to warn about.
func templateGitFileWarning(wsDir string) string {
	info, err := os.Lstat(filepath.Join(wsDir, ".git"))
	if err != nil || info.IsDir() {
		return ""
	}
	return "template .git is a file (git worktree or submodule): the instance shares the template's git dir, so commits inside it change the template's repo"
}
