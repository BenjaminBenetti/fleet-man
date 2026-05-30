package create

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
)

// applyMountSymlinks runs a shell script inside the just-created
// container to materialise each Symlink in symlinks. The script for
// each link, in order:
//
//  1. Migrates any pre-baked file at the symlink target into the
//     shared host file when the host file is still empty — preserving
//     agent state shipped in the image (e.g. a stub ~/.claude.json).
//  2. Replaces the target with a symlink pointing at the shared file.
//  3. If the shared file is still empty AND the link declares a
//     SeedContent, writes that seed so apps that parse the file on
//     startup see a valid initial value (Claude Code, for instance,
//     crashes if ~/.claude.json is not valid JSON).
func applyMountSymlinks(instanceBackend backend.Backend, wsDir string, symlinks []mountresolver.Symlink) error {
	if len(symlinks) == 0 {
		return nil
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, link := range symlinks {
		fmt.Fprintf(&script, `mkdir -p %s
if [ -e %s ] && [ ! -L %s ] && [ ! -s %s ]; then
  cp %s %s
fi
ln -sf %s %s
`,
			dotfiles.ShQuote(filepath.Dir(link.Target)),
			dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Source),
			dotfiles.ShQuote(link.Target), dotfiles.ShQuote(link.Source),
			dotfiles.ShQuote(link.Source), dotfiles.ShQuote(link.Target),
		)
		if link.SeedContent != "" {
			fmt.Fprintf(&script, `if [ ! -s %s ]; then
  printf '%%s' %s > %s
fi
`,
				dotfiles.ShQuote(link.Source),
				dotfiles.ShQuote(link.SeedContent),
				dotfiles.ShQuote(link.Source),
			)
		}
	}
	cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", script.String()})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
