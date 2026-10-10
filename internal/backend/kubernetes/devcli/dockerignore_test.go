package devcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerignoreMatching(t *testing.T) {
	rules := mustRules(t, `
# comment
*.pem
!keep.pem
secrets/
/build
**/id_*
docs/**/*.env
a?c
[x-z].key
`)
	for path, want := range map[string]bool{
		"server.pem":           true,
		"keep.pem":             false,
		"sub/server.pem":       false, // *.pem is anchored at the root
		"secrets/a":            true,  // below an excluded directory
		"secrets/deep/b":       true,
		"build":                true,
		"build/out":            true,
		"src/build":            false,
		"id_rsa":               true,
		"home/.ssh/id_ed25519": true,
		"docs/x.env":           true,
		"docs/a/b/x.env":       true,
		"docs/x.envy":          false,
		"abc":                  true,
		"a/c":                  false, // ? never matches a separator
		"y.key":                true,
		"w.key":                false,
		"README.md":            false,
	} {
		if got := rules.excludes(path); got != want {
			t.Errorf("excludes(%q) = %v, want %v", path, got, want)
		}
	}
	all := mustRules(t, "**\n!keep\n")
	if !all.excludes("any/thing") || all.excludes("keep") {
		t.Errorf("** then !keep")
	}
}

func mustRules(t *testing.T, content string) *ignoreRules {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dockerignore")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rules, err := parseDockerignore(f)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestBuildContextWarnings(t *testing.T) {
	mk := func(t *testing.T, files ...string) string {
		dir := t.TempDir()
		for _, f := range files {
			path := filepath.Join(dir, f)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			content := ""
			if strings.HasSuffix(f, ".dockerignore") {
				content = ".env\nkeys/\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cfgFor := func(ctx string) *Config {
		return &Config{Build: &Build{Context: ctx, Dockerfile: filepath.Join(ctx, ".devcontainer", "Dockerfile")}}
	}

	if w := BuildContextWarnings(&Config{}); w != nil {
		t.Errorf("an image-only configuration: %q", w)
	}
	if w := BuildContextWarnings(nil); w != nil {
		t.Errorf("nil: %q", w)
	}

	clean := mk(t, ".dockerignore", "src/main.go", "id_rsa.pub")
	if w := BuildContextWarnings(cfgFor(clean)); len(w) != 0 {
		t.Errorf("a clean context: %q", w)
	}

	bare := mk(t, "src/main.go")
	if w := BuildContextWarnings(cfgFor(bare)); len(w) != 1 || !strings.Contains(w[0], "has no .dockerignore") {
		t.Errorf("no .dockerignore: %q", w)
	}

	leaky := mk(t, ".dockerignore", ".env", "keys/id_rsa", "deploy/id_ed25519", "tls/cert.pem", ".git/id_rsa")
	w := BuildContextWarnings(cfgFor(leaky))
	if len(w) != 1 || !strings.Contains(w[0], "(deploy/id_ed25519, tls/cert.pem)") {
		t.Errorf("secrets the .dockerignore misses (and none of .git): %q", w)
	}

	// BuildKit's <Dockerfile>.dockerignore counts as the context's.
	perDockerfile := mk(t, ".devcontainer/Dockerfile.dockerignore", ".env")
	if w := BuildContextWarnings(cfgFor(perDockerfile)); len(w) != 0 {
		t.Errorf("a Dockerfile.dockerignore: %q", w)
	}

	many := mk(t, "a.pem", "b.pem", "c.pem", "d.pem", "e.pem", "f.pem", "g.pem")
	w = BuildContextWarnings(cfgFor(many))
	if len(w) != 2 || !strings.Contains(w[1], "e.pem, 2 more)") {
		t.Errorf("a long list is cut: %q", w)
	}
}

func TestLooksSecret(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, "id_rsa": true, "id_ed25519": true, "id_rsa.pub": false, "server.pem": true,
		".env.example": false, "env": false, "rsa_id": false, "main.go": false,
	} {
		if got := looksSecret(name); got != want {
			t.Errorf("looksSecret(%q) = %v", name, got)
		}
	}
}
