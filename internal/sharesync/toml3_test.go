package sharesync

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// codexConfig is a Codex config.toml as a user keeps it: comments, a
// trailing comment, a multi-line array, a quoted table name.
const codexConfig = `# Codex config
model = "o3" # my favourite
approval_policy = "on-request"
notify = [
  "notify-send", # the desktop
  "Codex",
]

# Projects I trust.
[projects."/workspaces/a"]
trust_level = "trusted"

[tui]
# the status line
theme = "dark"
`

// TestTOML3Decisions pins toml3's table: fast forwards taken, a file that
// does not parse or is empty left waiting, two changed copies merged.
func TestTOML3Decisions(t *testing.T) {
	ctx := context.Background()
	c := docCase{path: "config.toml", cur: "a = 1\n", inc: "a = 2\n", fast: true}
	if res, err := (TOML3{}).Merge(ctx, c.input()); err != nil || res.Decision != Accept {
		t.Errorf("fast forward: %+v, %v", res, err)
	}
	for _, inc := range []string{"", "a = ", "[t\nb = 1\n", "a = 1\na = 2\n", `s = "x`} {
		for _, fast := range []bool{true, false} {
			c := docCase{path: "config.toml", cur: "a = 1\n", inc: inc, fast: fast}
			if _, err := (TOML3{}).Merge(ctx, c.input()); !errors.Is(err, ErrIncomplete) {
				t.Errorf("%q (fast %v): %v, want ErrIncomplete", inc, fast, err)
			}
		}
	}
	// A file of comments only is a whole (empty) config, and no seed.
	c = docCase{path: "config.toml", cur: "a = 1\n", inc: "# nothing\n", fast: true}
	if res, err := (TOML3{}).Merge(ctx, c.input()); err != nil || res.Decision != Accept {
		t.Errorf("a config of comments: %+v, %v", res, err)
	}
	c = docCase{path: "config.toml", base: "a = 1\nb = 1\n", cur: "a = 2\nb = 1\n", inc: "a = 1\nb = 2\n"}
	res, err := TOML3{}.Merge(ctx, c.input())
	if err != nil || res.Decision != Merged || string(res.Content) != "a = 2\nb = 2\n" {
		t.Errorf("both changed other keys: %+v %q, %v", res, res.Content, err)
	}
	// TOML is typed: 1 and 1.0 are two values.
	c = docCase{path: "config.toml", base: "a = 1\n", cur: "a = 1.0\n", inc: "a = 1\nb = 1\n"}
	if res, _ := (TOML3{}).Merge(ctx, c.input()); string(res.Content) != "a = 1.0\nb = 1\n" {
		t.Errorf("a type change: %+v %q", res, res.Content)
	}
}

// TestTOML3KeepsTheCurrentCopy checks a merged config is the current copy
// with the other's changes written in: comments, blank lines, order,
// quoting and trailing comments stay, a changed value is replaced in
// place, a removed key goes, a new key lands after its table's last key, a
// new table at the end.
func TestTOML3KeepsTheCurrentCopy(t *testing.T) {
	cases := []struct {
		name, base, cur, inc, want string
	}{
		{
			name: "a project trusted elsewhere",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
			inc:  codexConfig + "\n[projects.\"/workspaces/b\"]\ntrust_level = \"trusted\"\n",
			want: strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1) + "\n[projects.\"/workspaces/b\"]\ntrust_level = 'trusted'\n",
		},
		{
			name: "a value changed in place keeps its comment",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
			inc:  strings.Replace(codexConfig, `model = "o3"`, `model = "gpt-5"`, 1),
			want: strings.Replace(strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1), `model = "o3"`, `model = 'gpt-5'`, 1),
		},
		{
			name: "a multi-line array replaced",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
			inc:  strings.Replace(codexConfig, `"Codex",`, `"Codex", "done",`, 1),
			want: strings.Replace(strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
				"notify = [\n  \"notify-send\", # the desktop\n  \"Codex\",\n]", "notify = ['notify-send', 'Codex', 'done']", 1),
		},
		{
			name: "a removed key goes, a new one follows its table's last",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
			inc:  strings.Replace(strings.Replace(codexConfig, "approval_policy = \"on-request\"\n", "", 1), "[tui]\n", "[tui]\nalt = true\n", 1) + "",
			want: strings.Replace(strings.Replace(codexConfig, `theme = "dark"`, "theme = \"light\"\nalt = true", 1), "approval_policy = \"on-request\"\n", "", 1),
		},
		{
			name: "a new root key after the root table's last",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1),
			inc:  strings.Replace(codexConfig, "approval_policy", "sandbox_mode = \"workspace-write\"\napproval_policy", 1),
			want: strings.Replace(strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1), "\"Codex\",\n]\n", "\"Codex\",\n]\nsandbox_mode = 'workspace-write'\n", 1),
		},
		{
			name: "a table removed",
			base: codexConfig,
			cur:  strings.Replace(codexConfig, `model = "o3"`, `model = "o4"`, 1),
			inc:  strings.Replace(codexConfig, "[tui]\n# the status line\ntheme = \"dark\"\n", "", 1),
			want: strings.Replace(strings.Replace(codexConfig, `model = "o3"`, `model = "o4"`, 1), "[tui]\n# the status line\ntheme = \"dark\"\n", "", 1),
		},
		{
			name: "dotted keys extended where they are",
			base: "a.b = 1\nz = 0\n",
			cur:  "a.b = 1\nz = 1\n",
			inc:  "a.b = 1\na.c = 2\nz = 0\n",
			want: "a.b = 1\na.c = 2\nz = 1\n",
		},
		{
			name: "indented tables keep their indent",
			base: "[t]\n  a = 1\n",
			cur:  "[t]\n  a = 2\n",
			inc:  "[t]\n  a = 1\n  b = 1\n",
			want: "[t]\n  a = 2\n  b = 1\n",
		},
		{
			name: "a config without a final newline",
			base: "a = 1",
			cur:  "a = 2",
			inc:  "a = 1\nb = 1",
			want: "a = 2\nb = 1\n",
		},
		{
			name: "multi-line strings and quoted keys",
			base: "s = \"\"\"\nx = [\n\"\"\"\n'k.#' = 1\n",
			cur:  "s = \"\"\"\nx = [\n\"\"\"\n'k.#' = 2\n",
			inc:  "s = \"\"\"\nx = [\n\"\"\"\n'k.#' = 1\nn = 1\n",
			want: "s = \"\"\"\nx = [\n\"\"\"\n'k.#' = 2\nn = 1\n",
		},
	}
	for _, tc := range cases {
		res, err := TOML3{}.Merge(context.Background(), docCase{path: "config.toml", base: tc.base, cur: tc.cur, inc: tc.inc}.input())
		if err != nil || res.Decision != Merged {
			t.Errorf("%s: %+v, %v", tc.name, res, err)
			continue
		}
		if string(res.Content) != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, res.Content, tc.want)
		}
	}
}

// TestTOML3WritesPlainWhenItCannotPatch checks a layout the patch does not
// handle (arrays of tables) still merges, written out plain.
func TestTOML3WritesPlainWhenItCannotPatch(t *testing.T) {
	base := "# servers\n[[srv]]\nname = \"a\"\n"
	cur := "# servers\n[[srv]]\nname = \"a\"\n[[srv]]\nname = \"b\"\n"
	inc := "# servers\nx = 1\n[[srv]]\nname = \"a\"\n"
	res, err := TOML3{}.Merge(context.Background(), docCase{path: "config.toml", base: base, cur: cur, inc: inc}.input())
	if err != nil || res.Decision != Merged {
		t.Fatalf("%+v, %v", res, err)
	}
	got, err := parseTOML(res.Content)
	want, _ := parseTOML([]byte("x = 1\n[[srv]]\nname = \"a\"\n[[srv]]\nname = \"b\"\n"))
	if err != nil || !same(got, want) {
		t.Errorf("merged %q, %v", res.Content, err)
	}
}

// TestSplitTOML checks the statement split covers the document exactly,
// with each statement's path.
func TestSplitTOML(t *testing.T) {
	stmts, err := splitTOML([]byte(codexConfig))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var paths []string
	for _, st := range stmts {
		b.WriteString(codexConfig[st.start:st.end])
		if st.kind != tomlTrivia {
			paths = append(paths, strings.Join(st.path, "/"))
		}
	}
	if b.String() != codexConfig {
		t.Errorf("split does not cover the document:\n%s", b.String())
	}
	want := "model approval_policy notify projects//workspaces/a projects//workspaces/a/trust_level tui tui/theme"
	if got := strings.Join(paths, " "); got != want {
		t.Errorf("paths %s\nwant  %s", got, want)
	}
}
