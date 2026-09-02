package create

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// copyTemplateTree materialises a template fleet's workspace: the directory a
// file:// remote points at is copied (cp -a, so permissions, mtimes, symlinks
// and any .git directory survive) into wsDir. It is the template counterpart of
// `git clone <remote> <wsDir>` — every new instance starts from a pristine copy
// of the template, and edits inside an instance never touch the template.
func copyTemplateTree(remoteURL, wsDir string) error {
	dir, err := fleet.TemplateDir(remoteURL)
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("template dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("template path %s is not a directory", dir)
	}
	if err := copyWorkspaceTree(dir, wsDir); err != nil {
		return fmt.Errorf("copy template %s: %w", dir, err)
	}
	return nil
}
