package agentstrategy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

func TestStatusModShipsTheModButNotItsTests(t *testing.T) {
	setup, err := statusModSetup("/tmp/claude-mod")
	if err != nil {
		t.Fatal(err)
	}
	root := "/tmp/claude-mod/fleet-status"
	var paths []string
	files := map[string]File{}
	for _, f := range setup.Files {
		paths = append(paths, strings.TrimPrefix(f.Path, root+"/"))
		files[f.Path] = f
		if f.Mode != 0o644 {
			t.Errorf("%s mode = %v, want 0644", f.Path, f.Mode)
		}
	}
	slices.Sort(paths)
	want := []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.tsx", "hooks/view.ts", "types/index.d.ts"}
	if !slices.Equal(paths, want) {
		t.Fatalf("files = %v, want %v", paths, want)
	}

	var manifest struct{ Name, Version, Types string }
	if err := json.Unmarshal(files[root+"/.claude-plugin/plugin.json"].Content, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != StatusModName || manifest.Version != pluginVersion() || manifest.Types != "./types/index.d.ts" {
		t.Fatalf("manifest = %+v, want %s at fleet's version with its state contract", manifest, StatusModName)
	}
	if len(setup.Env) != 1 || setup.Env[0].Name != "CLAUDE_CODE_PLUGIN_DIRS" || setup.Env[0].Value != root || !setup.Env[0].PathList {
		t.Fatalf("env = %+v, want CLAUDE_CODE_PLUGIN_DIRS=%s prepended", setup.Env, root)
	}
}

func TestInstallStatusModOnlyRewritesWhatChanged(t *testing.T) {
	dir := t.TempDir()
	exports, err := InstallStatusMod(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, StatusModName)
	if !strings.Contains(exports, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(exports, root) {
		t.Fatalf("exports = %q, want CLAUDE_CODE_PLUGIN_DIRS pointed at %s", exports, root)
	}

	// A running session reloads the mod on every write to its folder, so a
	// shell starting must leave unchanged files alone.
	module := filepath.Join(root, "hooks", "register.tsx")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(module, old, old); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(root, "hooks", "view.ts")
	if err := os.WriteFile(view, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStatusMod(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(module); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("an unchanged file was rewritten (%v)", err)
	}
	if got, _ := os.ReadFile(view); string(got) == "stale" {
		t.Fatal("a changed file was not restored")
	}
}

// The mod reads what the daemon writes: hold the Go schema and the
// TypeScript reader to the same field names and the same path.
func TestStatusModFileMatchesTheModsReader(t *testing.T) {
	src, err := statusModSource.ReadFile("statusmod/hooks/view.ts")
	if err != nil {
		t.Fatal(err)
	}
	fieldsOf := func(typeName string) []string {
		m := regexp.MustCompile(`(?s)export type ` + typeName + ` = \{(.*?)\}`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("view.ts declares no %s", typeName)
		}
		var fields []string
		for _, f := range regexp.MustCompile(`(\w+):`).FindAllSubmatch(m[1], -1) {
			fields = append(fields, string(f[1]))
		}
		slices.Sort(fields)
		return fields
	}
	jsonKeys := func(v any) []string {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}

	if got, want := jsonKeys(StatusModFile{}), fieldsOf("Status"); !slices.Equal(got, want) {
		t.Errorf("StatusModFile fields %v, view.ts Status %v", got, want)
	}
	if got, want := jsonKeys(StatusModStop{}), fieldsOf("StatusStop"); !slices.Equal(got, want) {
		t.Errorf("StatusModStop fields %v, view.ts StatusStop %v", got, want)
	}
	path := control.ContainerMountDir + "/" + StatusModFileName
	if !strings.Contains(string(src), `STATUS_FILE = '`+path+`'`) {
		t.Errorf("view.ts must read the status file at %s", path)
	}
}
