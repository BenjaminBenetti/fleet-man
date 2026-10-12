package resolver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// No instance may be named after a folder the fleet keeps beside its
// instances' checkouts in ~/.fleet/workspaces/<fleet>: its checkout would
// land inside it (and, for a synced root, in every pod of the fleet).
func TestInstanceNamesCannotBeTheFleetsFolders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folders := []string{sharedFilesSubdir, customMountHostSubdir, hostbind.Root, podseed.SharedVolumesDir}
	for _, root := range syncpolicy.Defaults().Roots() {
		folders = append(folders, strings.SplitN(root.Path, "/", 2)[0])
	}
	fleetDir := filepath.Join(state.WorkspacesDir(), "alpha")
	for _, dir := range []string{state.BuildkitDir("alpha"), state.DebCacheDir("alpha"), state.ImageCacheDir("alpha")} {
		rel, err := filepath.Rel(fleetDir, dir)
		if err != nil {
			t.Fatal(err)
		}
		folders = append(folders, strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
	}
	for _, name := range folders {
		if err := fleet.ValidateInstanceName(name); err == nil {
			t.Errorf("an instance may be named %q, a folder of the fleet's", name)
		}
	}
}
