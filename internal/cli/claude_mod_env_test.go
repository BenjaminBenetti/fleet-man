package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeModEnv: `fleet claude-mod-env` writes the fleet status mod under the
// user's cache directory and prints exports that load it.
func TestClaudeModEnv(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	out, err := runCLI(t, "claude-mod-env")
	if err != nil {
		t.Fatalf("claude-mod-env: %v", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(cacheDir, "fleet", "claude-mod", "fleet-status")
	if !strings.Contains(out, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(out, mod) {
		t.Fatalf("exports = %q, want CLAUDE_CODE_PLUGIN_DIRS pointing at %s", out, mod)
	}
	for _, f := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.tsx"} {
		if _, err := os.Stat(filepath.Join(mod, f)); err != nil {
			t.Errorf("the mod's %s was not written: %v", f, err)
		}
	}
}
