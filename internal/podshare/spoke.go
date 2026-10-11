package podshare

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// SpokeVersion is the spoke spec format this fleet writes and reads.
const SpokeVersion = 1

// SpokeSpec is what fleetd asks of a pod's spoke: the header of the
// TagSyncCtl stream it opens on the pod's agent session, which starts the
// spoke (or reaches the running one).
type SpokeSpec struct {
	V int `json:"v"`
	// Roots are the tree's synced folders, each one sharesync root (a
	// policy table root: ".claude", ".config/gh", "files", ".mnt").
	Roots []SpokeRoot `json:"roots"`
	// UID and GID are the share uid the spoke runs as once it has what only
	// root may open; -1 keeps the agent's (a pod without root).
	UID int `json:"uid"`
	GID int `json:"gid"`
	// Pod is the pod's UID, written into the lease with its epoch.
	Pod string `json:"pod,omitempty"`
	// NoWatch polls the folders instead of watching them (tests).
	NoWatch bool `json:"noWatch,omitempty"`
	// Rescan overrides how often the folders are scanned in full ("" is
	// sharesync's minute), a Go duration.
	Rescan string `json:"rescan,omitempty"`
}

// SpokeRoot is one synced folder of the tree: RootsDir/Name.
type SpokeRoot struct {
	Name string `json:"name"`
	// Reserved are the layout's paths below the folder, which the spoke
	// never removes, renames or replaces.
	Reserved []string `json:"reserved,omitempty"`
	// Skip are folders below it the spoke leaves alone (another root's).
	Skip []string `json:"skip,omitempty"`
	// Only, when set, are the names directly in the folder the spoke syncs
	// (a bound file's folder holds just it: internal/mounts/hostbind).
	Only []string `json:"only,omitempty"`
}

// Dir is the root's folder relative to the tree's root.
func (r SpokeRoot) Dir() string { return path.Join(RootsDir, r.Name) }

// Validate checks s is a spec this fleet can run.
func (s SpokeSpec) Validate() error {
	if s.V > SpokeVersion {
		return fmt.Errorf("spoke spec version %d is newer than this fleet's %d", s.V, SpokeVersion)
	}
	if len(s.Roots) == 0 {
		return fmt.Errorf("spoke spec names no folders")
	}
	seen := map[string]bool{}
	for _, r := range s.Roots {
		if !validPath(r.Name) || seen[r.Name] {
			return fmt.Errorf("spoke folder %q is not a clean path below the tree's roots, or named twice", r.Name)
		}
		seen[r.Name] = true
		for _, p := range append(slices.Clone(r.Reserved), r.Skip...) {
			if !validPath(p) {
				return fmt.Errorf("spoke folder %s: path %q is not a clean path below it", r.Name, p)
			}
		}
		for _, name := range r.Only {
			if !validPath(name) || strings.Contains(name, "/") {
				return fmt.Errorf("spoke folder %s: %q is not a name in it", r.Name, name)
			}
		}
	}
	if (s.UID < 0) != (s.GID < 0) {
		return fmt.Errorf("spoke owner %d:%d sets one id without the other", s.UID, s.GID)
	}
	if s.Rescan != "" {
		if d, err := time.ParseDuration(s.Rescan); err != nil || d <= 0 {
			return fmt.Errorf("spoke rescan %q is not a positive duration", s.Rescan)
		}
	}
	return nil
}

// LayoutRoots groups a layout's paths by the sharesync root covering each
// (root finds a path's root in the policy table: syncpolicy.Table.Root): one
// SpokeRoot per root a layout path is in, its layout paths below it as
// Reserved. A path no root covers is left out.
func LayoutRoots(entries []Entry, root func(p string) (syncpolicy.Root, string, bool)) []SpokeRoot {
	var out []SpokeRoot
	index := map[string]int{}
	for _, e := range entries {
		r, inner, ok := root(e.Path)
		if !ok {
			continue
		}
		name := r.Path
		i, have := index[name]
		if !have {
			i = len(out)
			index[name] = i
			out = append(out, SpokeRoot{Name: name})
		}
		if inner != "" && inner != "." && !slices.Contains(out[i].Reserved, inner) {
			out[i].Reserved = append(out[i].Reserved, inner)
		}
	}
	for i := range out {
		for j := range out {
			if i != j && strings.HasPrefix(out[j].Name, out[i].Name+"/") {
				out[i].Skip = append(out[i].Skip, strings.TrimPrefix(out[j].Name, out[i].Name+"/"))
			}
		}
	}
	return out
}
