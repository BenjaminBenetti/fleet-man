package podseed

import (
	"path"
	"slices"
	"sort"
	"strings"
)

// alwaysExcluded are top-level dirs no seed copies: the kernel's and the
// runtime's, recreated in every container.
var alwaysExcluded = []string{"proc", "sys", "dev"}

// topKind is what a top-level entry of the image root is.
type topKind int

const (
	// topDir is a real directory on the root filesystem: copied.
	topDir topKind = iota
	// topMount is a directory something is mounted on in the seed pod (the
	// PVC, the tools dir, a kubelet mount): never copied.
	topMount
	// topOther is a symlink (/bin -> usr/bin), a file or a special file:
	// part of the image, not persisted.
	topOther
)

// rootPlan is what a seed copies and creates for one root generation.
type rootPlan struct {
	// dirs are the image's top-level dirs to copy into root/.
	dirs []string
	// extra are the top-level dirs to create under root-extra/.
	extra []string
	// mountpoints are the mount targets inside copied dirs: their dir is
	// created, their image content is not copied (the mount hides it).
	mountpoints map[string]bool
	// warnings are mount targets the plan cannot honour.
	warnings []string
}

// planRoot decides a generation's layout from the image's top-level entries
// and the instance's mount targets.
func planRoot(tops map[string]topKind, spec SeedSpec) rootPlan {
	plan := rootPlan{mountpoints: map[string]bool{}}
	excluded := map[string]bool{}
	for _, name := range append(slices.Clone(alwaysExcluded), spec.Exclude...) {
		excluded[name] = true
	}
	for name, kind := range tops {
		if kind == topDir && !excluded[name] {
			plan.dirs = append(plan.dirs, name)
		}
	}
	sort.Strings(plan.dirs)

	extra := map[string]bool{}
	for _, target := range outermost(spec.MountDirs) {
		top, _, _ := strings.Cut(strings.TrimPrefix(target, "/"), "/")
		if excluded[top] {
			continue
		}
		switch kind, ok := tops[top]; {
		case !ok:
			extra[top] = true
		case kind == topDir:
			plan.mountpoints[target] = true
		case kind == topMount:
			// The seed pod's own mount is no part of the image: the target's
			// parent is new to the instance.
			extra[top] = true
		default:
			plan.warnings = append(plan.warnings, "mount target "+target+": /"+top+" is not a directory in the image")
		}
	}
	for top := range extra {
		plan.extra = append(plan.extra, top)
	}
	sort.Strings(plan.extra)
	return plan
}

// outermost is dirs without the ones nested inside another: a nested mount's
// mountpoint is its parent mount's to provide.
func outermost(dirs []string) []string {
	clean := make([]string, 0, len(dirs))
	for _, d := range dirs {
		clean = append(clean, path.Clean(d))
	}
	sort.Strings(clean)
	var out []string
	for _, d := range clean {
		nested := false
		for _, o := range out {
			if d == o || strings.HasPrefix(d, o+"/") {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, d)
		}
	}
	return out
}
