package devcontainer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestNeutralizeMissingDevcontainerIsNoop verifies that a workspace
// without a devcontainer.json still produces a callable restore func
// (so the caller's `defer restore()` is always safe) and never touches
// the filesystem.
func TestNeutralizeMissingDevcontainerIsNoop(t *testing.T) {
	dir := t.TempDir()
	restore, err := neutralizeConflictingMounts(dir, []backend.Mount{
		{LocalPath: "/host/.claude", ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("neutralize on missing config = %v, want nil", err)
	}
	if restore == nil {
		t.Fatal("restore is nil; callers cannot defer it")
	}
	restore()
}

// TestNeutralizeNoConflictLeavesFileUntouched verifies the
// optimisation: when no fleet mount targets any of the user's declared
// mounts, the file content is left byte-identical so a deferred
// restore is unnecessary.
func TestNeutralizeNoConflictLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".devcontainer", "devcontainer.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{
  // user comment
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "mounts": [
    "source=/host/secrets,target=/run/secrets,type=bind"
  ]
}`)
	if err := os.WriteFile(configPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	restore, err := neutralizeConflictingMounts(dir, []backend.Mount{
		{LocalPath: "/host/.claude", ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("neutralize = %v, want nil", err)
	}
	restore()

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("file was modified despite no conflict:\nwant: %s\ngot:  %s", original, got)
	}
}

// TestStripConflictingMountsStringForm covers the docker-mount-string
// syntax (`source=...,target=...,type=bind`) for all three legal
// destination keys: target, dst, destination.
func TestStripConflictingMountsStringForm(t *testing.T) {
	tests := []struct {
		name      string
		entry     string
		fleetPath string
		wantKeep  bool
	}{
		{
			name:      "target= conflicts",
			entry:     "source=/host/.claude,target=/home/vscode/.claude,type=bind",
			fleetPath: "/home/vscode/.claude",
			wantKeep:  false,
		},
		{
			name:      "dst= conflicts",
			entry:     "type=bind,src=/host/.codex,dst=/home/vscode/.codex",
			fleetPath: "/home/vscode/.codex",
			wantKeep:  false,
		},
		{
			name:      "destination= conflicts",
			entry:     "source=/host/x,destination=/home/vscode/.claude,type=bind",
			fleetPath: "/home/vscode/.claude",
			wantKeep:  false,
		},
		{
			name:      "non-matching target is kept",
			entry:     "source=/host/secrets,target=/run/secrets,type=bind",
			fleetPath: "/home/vscode/.claude",
			wantKeep:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte(`{"mounts":["` + tt.entry + `"]}`)
			rewritten, removed, err := stripConflictingMounts(original, []backend.Mount{
				{ContainerPath: tt.fleetPath},
			})
			if err != nil {
				t.Fatalf("stripConflictingMounts = %v, want nil", err)
			}
			if tt.wantKeep {
				if len(removed) != 0 {
					t.Fatalf("removed = %v, want empty", removed)
				}
				return
			}
			if len(removed) != 1 || removed[0] != tt.fleetPath {
				t.Fatalf("removed = %v, want [%s]", removed, tt.fleetPath)
			}
			if strings.Contains(string(rewritten), tt.entry) {
				t.Fatalf("rewritten still contains the conflicting entry: %s", rewritten)
			}
		})
	}
}

// TestStripConflictingMountsObjectForm covers the object-style mount
// syntax that devcontainer.json also accepts.
func TestStripConflictingMountsObjectForm(t *testing.T) {
	original := []byte(`{
  "mounts": [
    {"source": "/host/.claude", "target": "/home/vscode/.claude", "type": "bind"},
    {"source": "/host/keep", "destination": "/opt/keep", "type": "bind"}
  ]
}`)
	rewritten, removed, err := stripConflictingMounts(original, []backend.Mount{
		{ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("stripConflictingMounts = %v, want nil", err)
	}
	if len(removed) != 1 || removed[0] != "/home/vscode/.claude" {
		t.Fatalf("removed = %v, want [/home/vscode/.claude]", removed)
	}
	if strings.Contains(string(rewritten), "/home/vscode/.claude") {
		t.Fatalf("rewritten still references the stripped target: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), "/opt/keep") {
		t.Fatalf("rewritten dropped the unrelated mount: %s", rewritten)
	}
}

// TestStripConflictingMountsTolerantOfJSONC verifies the JSONC
// preprocessor (line/block comments, trailing commas) so users with
// real-world devcontainer.json files do not see their file silently
// left alone because of a parse error.
func TestStripConflictingMountsTolerantOfJSONC(t *testing.T) {
	original := []byte(`{
  // a line comment
  "image": "ubuntu", /* a block comment */
  "mounts": [
    "source=/host/.claude,target=/home/vscode/.claude,type=bind", // trailing line comment
  ],
}`)
	_, removed, err := stripConflictingMounts(original, []backend.Mount{
		{ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("stripConflictingMounts on JSONC = %v, want nil", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want one entry", removed)
	}
}

// TestNeutralizeRestoresOriginalBytes is the load-bearing guarantee for
// callers: after restore() runs, the on-disk file matches the original
// byte-for-byte. Otherwise the user's repo carries a phantom diff and
// a `git commit -a` from inside the workspace would capture it.
func TestNeutralizeRestoresOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "devcontainer.json")
	// Include comments and trailing commas so the test catches any
	// accidental re-serialisation through json.Marshal.
	original := []byte(`{
  // keep me
  "image": "ubuntu",
  "mounts": [
    "source=/host/.claude,target=/home/vscode/.claude,type=bind",
  ],
}`)
	if err := os.WriteFile(configPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	restore, err := neutralizeConflictingMounts(dir, []backend.Mount{
		{LocalPath: "/fleet/.claude", ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("neutralize = %v, want nil", err)
	}

	stripped, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stripped), "/home/vscode/.claude") {
		t.Fatalf("file still contains the conflicting mount mid-Up: %s", stripped)
	}

	restore()

	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("restore() did not reproduce the original bytes:\nwant: %s\ngot:  %s", original, restored)
	}
}

// TestNeutralizePrefersDotDevcontainerSubdir verifies the search order
// matches the devcontainer CLI's: the canonical
// .devcontainer/devcontainer.json wins over a repo-root .devcontainer.json
// when both are present.
func TestNeutralizePrefersDotDevcontainerSubdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0755); err != nil {
		t.Fatal(err)
	}
	subdirConfig := filepath.Join(dir, ".devcontainer", "devcontainer.json")
	rootConfig := filepath.Join(dir, ".devcontainer.json")

	conflicting := []byte(`{"mounts":["source=/h,target=/home/vscode/.claude,type=bind"]}`)
	clean := []byte(`{"image":"ubuntu"}`)
	if err := os.WriteFile(subdirConfig, conflicting, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootConfig, clean, 0644); err != nil {
		t.Fatal(err)
	}

	restore, err := neutralizeConflictingMounts(dir, []backend.Mount{
		{ContainerPath: "/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("neutralize = %v, want nil", err)
	}
	defer restore()

	got, err := os.ReadFile(subdirConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "/home/vscode/.claude") {
		t.Fatalf("subdir config was not neutralised: %s", got)
	}

	rootGot, err := os.ReadFile(rootConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(rootGot) != string(clean) {
		t.Fatalf("root config was touched but should have been ignored:\nwant: %s\ngot:  %s", clean, rootGot)
	}
}

// TestMountEntryTargetUnknownShape pins the contract that an
// unrecognised entry shape (e.g. a number, or an object missing both
// target and destination) returns "" rather than mis-attributing it to
// a fleet mount and stripping unrelated user config.
func TestMountEntryTargetUnknownShape(t *testing.T) {
	cases := []any{
		42,
		nil,
		map[string]any{"source": "/h"},
		"source=/h,type=bind",
	}
	for _, entry := range cases {
		if got := mountEntryTarget(entry); got != "" {
			t.Errorf("mountEntryTarget(%v) = %q, want \"\"", entry, got)
		}
	}
}
