package resolver

import (
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// everyMountSettings turns on every FleetSettings *Mount toggle, found by
// reflection so a tool added later is covered without editing this test,
// plus nested custom mounts.
func everyMountSettings(t *testing.T) fleet.FleetSettings {
	t.Helper()
	settings := fleet.FleetSettings{CustomMounts: []string{"/opt", "/opt/data", "/var/cache/shared"}}
	value := reflect.ValueOf(&settings).Elem()
	toggles := 0
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if field.Type.Kind() == reflect.Bool && strings.HasSuffix(field.Name, "Mount") {
			value.Field(i).SetBool(true)
			toggles++
		}
	}
	if toggles == 0 {
		t.Fatal("found no FleetSettings *Mount toggles")
	}
	return settings
}

// TestSyncPolicyCoversEveryMount keeps internal/mounts/syncpolicy in step with
// the mounts the resolver makes, so nothing a fleet shares is synced by
// accident of a default:
//   - every directory mount lives under a sync root;
//   - every agent tool mount is a root of its own, named for the tool, with a
//     credential rule for its login file, so a new tool's login can never
//     fall through to mirror;
//   - every single-file mount has a rule of its own (they are seeded '{}');
//   - every tool root in the table is a mount the resolver makes.
//
// It is the add-a-tool checklist's sync step: a new toggle in
// dirMountSpecsFor fails here until syncpolicy/defaults.go has its root.
func TestSyncPolicyCoversEveryMount(t *testing.T) {
	settings := everyMountSettings(t)
	table := syncpolicy.Defaults()
	toolMounts := map[string]bool{}

	for _, spec := range dirMountSpecsFor(settings, defaultContainerHome) {
		subPath := filepath.ToSlash(spec.hostSubdir)
		root, _, ok := table.Root(subPath)
		if !ok {
			t.Errorf("%s mount (%s) has no sync root: add one to internal/mounts/syncpolicy/defaults.go", spec.name, subPath)
			continue
		}
		if spec.custom {
			if root.Tool != "" {
				t.Errorf("%s (%s) is synced as %s state", spec.name, subPath, root.Tool)
			}
			continue
		}
		toolMounts[subPath] = true
		if root.Path != subPath || root.Tool != spec.name {
			t.Errorf("%s mount (%s) is synced under root %s (tool %q): give it a root of its own named %q", spec.name, subPath, root.Path, root.Tool, spec.name)
			continue
		}
		credential := false
		for _, rule := range root.Rules {
			credential = credential || rule.Policy == syncpolicy.Credential
		}
		if !credential {
			t.Errorf("%s mount (%s) has no credential rule: name its login file in internal/mounts/syncpolicy/defaults.go so it never falls through to %s", spec.name, subPath, root.Default)
		}
	}

	for _, spec := range fileMountSpecsFor(settings, defaultContainerHome) {
		name := path.Join(sharedFilesSubdir, spec.filename)
		root, inner, ok := table.Root(name)
		if !ok {
			t.Errorf("%s (%s) has no sync root", spec.name, name)
			continue
		}
		if _, ok := root.RuleFor(inner); !ok {
			t.Errorf("%s (%s) falls through to %s: give it a rule in internal/mounts/syncpolicy/defaults.go", spec.name, name, root.Default)
		}
	}

	for _, root := range table.Roots() {
		if root.Tool != "" && !toolMounts[root.Path] {
			t.Errorf("sync root %s (%s) is not a mount the resolver makes", root.Path, root.Tool)
		}
	}
}
