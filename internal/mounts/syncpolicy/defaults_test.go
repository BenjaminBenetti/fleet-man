package syncpolicy

import (
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
)

// TestDefaultsPolicies pins the plan's policy table (shared state, "Hub"):
// the four agent logins are credentials, the settings files merge, the
// histories union, transcripts and user content mirror, and the per-location
// paths stay put.
func TestDefaultsPolicies(t *testing.T) {
	cases := map[string]Policy{
		".claude/.credentials.json":                Credential,
		".codex/auth.json":                         Credential,
		".config/gh/hosts.yml":                     Credential,
		".augment/session.json":                    Credential,
		".claude/settings.json":                    JSON3,
		"files/.claude.json":                       JSON3,
		"files/claude-settings.local.json":         JSON3,
		".codex/config.toml":                       TOML3,
		".claude/history.jsonl":                    AppendUnion,
		".codex/history.jsonl":                     AppendUnion,
		".claude":                                  Mirror,
		".claude/projects":                         Mirror,
		".claude/projects/-workspaces-x/a.jsonl":   Mirror,
		".claude/todos/t.json":                     Mirror,
		".config/gh/config.yml":                    Mirror,
		".augment/settings.json":                   Mirror,
		".mnt/opt/data/report.csv":                 Mirror,
		".mnt/opt/data/Cargo.lock":                 Mirror,
		".mnt/web/yarn.lock":                       Mirror,
		".hostbind/1a2b3c4d/config":                Mirror,
		".claude/downloads/claude-2.1.0":           LocationLocal,
		".claude/ide/41231.lock":                   LocationLocal,
		".claude/daemon/pid":                       LocationLocal,
		".claude/packages/x/y":                     LocationLocal,
		".claude/sessions/s":                       LocationLocal,
		".codex/sessions/2026/10/11/rollout.jsonl": LocationLocal,
		".codex/state_5.sqlite":                    LocationLocal,
		".codex/state_5.sqlite-wal":                LocationLocal,
		".codex/sqlite/logs.sqlite":                LocationLocal,
		".codex/app-server-1234.sock":              LocationLocal,
		".augment/downloads/x":                     LocationLocal,
		".config/gh/state.lock":                    LocationLocal,
	}
	table := Defaults()
	for name, want := range cases {
		if got, ok := table.PolicyFor(name); !ok || got != want {
			t.Errorf("PolicyFor(%s) = %v, %v; want %v", name, got, ok, want)
		}
	}
	for _, name := range []string{".", ".config", ".buildkit/buildkitd.sock", "workspaces", ".claudex/x"} {
		if got, ok := table.PolicyFor(name); ok {
			t.Errorf("PolicyFor(%s) = %v; want no root", name, got)
		}
	}
}

// TestDefaultsCaps pins the per-root size caps: every agent tool's folder and
// the shared files folder have one, custom mounts and host binds have none
// (only the host disk limits them on local Docker), and Claude's transcripts
// carry a size warning.
func TestDefaultsCaps(t *testing.T) {
	const mib, gib = int64(1) << 20, int64(1) << 30
	want := map[string]int64{
		".claude": 8 * gib, ".codex": 4 * gib, ".config/gh": 64 * mib, ".augment": 2 * gib,
		"files": 64 * mib, ".mnt": 0, ".hostbind": 0,
	}
	roots := Defaults().Roots()
	if len(roots) != len(want) {
		t.Fatalf("%d roots, want %d", len(roots), len(want))
	}
	for _, root := range roots {
		if cap, ok := want[root.Path]; !ok || root.Cap != cap {
			t.Errorf("root %s: Cap %d, want %d", root.Path, root.Cap, cap)
		}
		for _, w := range root.Warnings {
			if w.Size <= 0 || root.Cap > 0 && w.Size >= root.Cap {
				t.Errorf("root %s: warning %+v is not below the cap", root.Path, w)
			}
		}
	}
	claude, _, _ := Defaults().Root(".claude")
	if len(claude.Warnings) != 1 || claude.Warnings[0] != (SizeWarning{Pattern: "projects/**", Size: 2 * gib}) {
		t.Errorf(".claude warnings = %+v", claude.Warnings)
	}
	w := claude.Warnings[0]
	for name, matches := range map[string]bool{"projects": true, "projects/-ws/a.jsonl": true, "todos/x": false, ".": false} {
		if w.Matches(name) != matches {
			t.Errorf("Matches(%s) = %v", name, !matches)
		}
	}
}

// TestDefaultsCredentials pins the login files the per-location store keeps.
func TestDefaultsCredentials(t *testing.T) {
	want := []string{".claude/.credentials.json", ".codex/auth.json", ".config/gh/hosts.yml", ".augment/session.json"}
	if got := Defaults().Credentials(); !slices.Equal(got, want) {
		t.Errorf("Credentials() = %q, want %q", got, want)
	}
}

// TestDefaultsAreWellFormed checks the table's shape: valid, unique roots;
// valid patterns; no Unset policy; a credential rule in every tool's root,
// ahead of the shared per-location rules.
func TestDefaultsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, root := range Defaults().Roots() {
		if root.Path == "." || !fs.ValidPath(root.Path) || seen[root.Path] {
			t.Errorf("root %q: invalid or duplicate path", root.Path)
		}
		seen[root.Path] = true
		if root.Default == Unset {
			t.Errorf("root %s: no default policy", root.Path)
		}
		credentials := 0
		for i, rule := range root.Rules {
			if rule.Policy == Unset || rule.Pattern == "" {
				t.Errorf("root %s rule %d: %+v", root.Path, i, rule)
			}
			for _, elem := range strings.Split(rule.Pattern, "/") {
				if _, err := path.Match(elem, ""); err != nil || elem == "" {
					t.Errorf("root %s rule %q: bad element %q: %v", root.Path, rule.Pattern, elem, err)
				}
			}
			if rule.Policy == Credential {
				credentials++
				if hasMeta(rule.Pattern) {
					t.Errorf("root %s: credential pattern %q is not a plain file", root.Path, rule.Pattern)
				}
				if first := firstShared(root.Rules); first >= 0 && i > first {
					t.Errorf("root %s: credential %q comes after the shared per-location rules", root.Path, rule.Pattern)
				}
			}
		}
		if root.Tool != "" && credentials == 0 {
			t.Errorf("tool root %s (%s) has no credential rule", root.Path, root.Tool)
		}
	}
}

// TestTableRootIsTheDeepest checks a path belongs to the deepest root holding
// it, and the path relative to that root.
func TestTableRootIsTheDeepest(t *testing.T) {
	table := Table{roots: []Root{
		{Path: ".mnt", Default: Mirror},
		{Path: ".mnt/opt/data", Default: LocationLocal},
	}}
	cases := map[string][2]string{
		".mnt":                {".mnt", "."},
		".mnt/opt":            {".mnt", "opt"},
		".mnt/opt/data":       {".mnt/opt/data", "."},
		".mnt/opt/data/x/y":   {".mnt/opt/data", "x/y"},
		".mnt/opt/database/x": {".mnt", "opt/database/x"},
	}
	for name, want := range cases {
		root, inner, ok := table.Root(name)
		if !ok || root.Path != want[0] || inner != want[1] {
			t.Errorf("Root(%s) = %s, %s, %v; want %s, %s", name, root.Path, inner, ok, want[0], want[1])
		}
	}
}

// TestMatch covers the pattern language, "**" at either end and between.
func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"auth.json", "auth.json", true},
		{"auth.json", "x/auth.json", false},
		{"projects/**", "projects", true},
		{"projects/**", "projects/a/b", true},
		{"projects/**", "projectsx/a", false},
		{"**/*.lock", "a.lock", true},
		{"**/*.lock", "a/b/c.lock", true},
		{"**/*.lock", "a/b/c.locked", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c/y", false},
		{"*.sqlite*", "state.sqlite-shm", true},
		{"*.sqlite*", "x/state.sqlite", false},
		{"[", "[", false},
	}
	for _, tc := range cases {
		if got := match(strings.Split(tc.pattern, "/"), strings.Split(tc.name, "/")); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
	root := Root{Rules: []Rule{{Pattern: "x", Policy: Credential}}, Default: Mirror}
	if _, ok := root.RuleFor("."); ok {
		t.Error("RuleFor(.) matched a rule")
	}
	if got := root.PolicyFor("."); got != Mirror {
		t.Errorf("PolicyFor(.) = %v, want the default", got)
	}
}

// TestPolicyString pins the names status output and logs use.
func TestPolicyString(t *testing.T) {
	want := map[Policy]string{
		Unset: "unset", Mirror: "mirror", Credential: "credential", JSON3: "json3",
		TOML3: "toml3", AppendUnion: "append-union", LocationLocal: "location-local",
		Policy(200): "unset",
	}
	for p, s := range want {
		if p.String() != s {
			t.Errorf("Policy(%d).String() = %q, want %q", p, p.String(), s)
		}
	}
}

// firstShared returns the index of the first agentLocal rule in rules, or -1.
func firstShared(rules []Rule) int {
	for i, rule := range rules {
		if slices.Contains(agentLocal, rule) {
			return i
		}
	}
	return -1
}
