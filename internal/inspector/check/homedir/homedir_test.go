package homedir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

// TestExtractUserFromConfigHandlesRemoteUser checks the simple, common
// case: a JSON object with a remoteUser field.
func TestExtractUserFromConfigHandlesRemoteUser(t *testing.T) {
	input := []byte(`{"name":"x","remoteUser":"vscode","image":"alpine"}`)
	if got := extractUserFromConfig(input); got != "vscode" {
		t.Fatalf("got %q, want %q", got, "vscode")
	}
}

// TestExtractUserFromConfigHandlesContainerUser falls back to the
// containerUser field when remoteUser is absent.
func TestExtractUserFromConfigHandlesContainerUser(t *testing.T) {
	input := []byte(`{"containerUser":"node"}`)
	if got := extractUserFromConfig(input); got != "node" {
		t.Fatalf("got %q, want %q", got, "node")
	}
}

// TestExtractUserFromConfigToleratesJSONCComments verifies the regex
// approach handles devcontainer.json files with line comments and
// trailing commas, which the standard encoding/json parser would
// reject.
func TestExtractUserFromConfigToleratesJSONCComments(t *testing.T) {
	input := []byte(`{
		// this is a JSONC comment
		"image": "alpine",
		"remoteUser": "developer", // who runs the container
	}`)
	if got := extractUserFromConfig(input); got != "developer" {
		t.Fatalf("got %q, want %q", got, "developer")
	}
}

// TestExtractUserFromConfigReturnsEmptyWhenAbsent makes sure callers
// can distinguish "no user field" from a real value.
func TestExtractUserFromConfigReturnsEmptyWhenAbsent(t *testing.T) {
	input := []byte(`{"name":"no-user","image":"alpine"}`)
	if got := extractUserFromConfig(input); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestExtractImageFromConfig verifies the image field is pulled out
// when present.
func TestExtractImageFromConfig(t *testing.T) {
	input := []byte(`{"image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`)
	want := "mcr.microsoft.com/devcontainers/base:ubuntu"
	if got := extractImageFromConfig(input); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestLastUserInMetadataPicksFinalEntry verifies the "last wins"
// merge order from the devcontainer spec. Microsoft's base images
// declare features first (no user) and the image-level config last
// (the real default user); we must pick the final occurrence.
func TestLastUserInMetadataPicksFinalEntry(t *testing.T) {
	label := []byte(`[
		{"id":"ghcr.io/devcontainers/features/common-utils:2"},
		{"id":"ghcr.io/devcontainers/features/go:1"},
		{"customizations":{},"remoteUser":"vscode"}
	]`)
	if got := lastUserInMetadata(label); got != "vscode" {
		t.Fatalf("got %q, want %q", got, "vscode")
	}
}

// TestLastUserInMetadataOverrideOrder confirms that a later remoteUser
// supersedes an earlier one when multiple entries declare it — the
// behavior the devcontainer CLI applies when composing features.
func TestLastUserInMetadataOverrideOrder(t *testing.T) {
	label := []byte(`[
		{"id":"feature-a","remoteUser":"node"},
		{"id":"feature-b","remoteUser":"vscode"}
	]`)
	if got := lastUserInMetadata(label); got != "vscode" {
		t.Fatalf("got %q, want %q", got, "vscode")
	}
}

// TestLastUserInMetadataEmptyWhenAbsent ensures a metadata label that
// declares no user returns the empty string (not an error / panic).
func TestLastUserInMetadataEmptyWhenAbsent(t *testing.T) {
	label := []byte(`[{"id":"feature-a"},{"id":"feature-b"}]`)
	if got := lastUserInMetadata(label); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestHomeForUserMapsRootToSlashRoot covers the special-case for the
// Docker default user.
func TestHomeForUserMapsRootToSlashRoot(t *testing.T) {
	cases := []string{"root", "0", "", "  "}
	for _, user := range cases {
		if got := homeForUser(user); got != "/root" {
			t.Errorf("homeForUser(%q) = %q, want %q", user, got, "/root")
		}
	}
}

// TestHomeForUserMapsUsernameToSlashHome covers the common case.
func TestHomeForUserMapsUsernameToSlashHome(t *testing.T) {
	if got := homeForUser("vscode"); got != "/home/vscode" {
		t.Fatalf("got %q, want %q", got, "/home/vscode")
	}
}

// TestHomeForUserStripsGroupSpec accepts USER directives of the form
// "name:group" or "uid:gid" by ignoring the group part.
func TestHomeForUserStripsGroupSpec(t *testing.T) {
	if got := homeForUser("node:1000"); got != "/home/node" {
		t.Fatalf("got %q, want %q", got, "/home/node")
	}
}

// TestDetectReturnsHomeForRemoteUser is the integration test for the
// fast path: a devcontainer.json with remoteUser set produces the
// matching home directory without ever shelling out to docker.
func TestDetectReturnsHomeForRemoteUser(t *testing.T) {
	repo := newFakeRepo(t, `{"remoteUser":"node"}`)

	got, err := Detect(repo)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if got != "/home/node" {
		t.Errorf("Detect() = %q, want %q", got, "/home/node")
	}
}

// TestDetectIgnoresCommentedOutRemoteUser verifies that a JSONC line
// comment containing what looks like a `remoteUser` field does not
// shadow the real, live setting beneath it. Without stripping
// comments first, the regex would greedily match the commented-out
// "root" and report the wrong home directory.
func TestDetectIgnoresCommentedOutRemoteUser(t *testing.T) {
	repo := newFakeRepo(t, `{
		// "remoteUser": "root",
		"remoteUser": "vscode"
	}`)

	got, err := Detect(repo)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if got != "/home/vscode" {
		t.Errorf("Detect() = %q, want %q", got, "/home/vscode")
	}
}

// TestDetectIgnoresBlockCommentedRemoteUser is the block-comment
// equivalent of the line-comment case above.
func TestDetectIgnoresBlockCommentedRemoteUser(t *testing.T) {
	repo := newFakeRepo(t, `{
		/* "remoteUser": "root", */
		"remoteUser": "node"
	}`)

	got, err := Detect(repo)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if got != "/home/node" {
		t.Errorf("Detect() = %q, want %q", got, "/home/node")
	}
}

// TestDetectReturnsErrNoUserHintWhenNothingMatches verifies the error
// path: a devcontainer.json with no remoteUser/containerUser/image
// fields surfaces ErrNoUserHint so the UI can ask the user to type a
// value.
func TestDetectReturnsErrNoUserHintWhenNothingMatches(t *testing.T) {
	repo := newFakeRepo(t, `{"name":"empty"}`)

	_, err := Detect(repo)
	if !errors.Is(err, ErrNoUserHint) {
		t.Fatalf("err = %v, want ErrNoUserHint", err)
	}
}

// TestDetectSurfacesNoDevcontainerConfig propagates the inspector's
// missing-config sentinel so callers can tell repos with no
// devcontainer.json apart from repos with an unparseable one.
func TestDetectSurfacesNoDevcontainerConfig(t *testing.T) {
	repo := &inspector.Repo{Root: t.TempDir()}

	_, err := Detect(repo)
	if !errors.Is(err, inspector.ErrNoDevcontainerConfig) {
		t.Fatalf("err = %v, want ErrNoDevcontainerConfig", err)
	}
}

// newFakeRepo writes contents to .devcontainer/devcontainer.json under
// a fresh temp dir and returns a Repo rooted there — bypassing the
// real git clone path so tests run hermetically.
func newFakeRepo(t *testing.T, contents string) *inspector.Repo {
	t.Helper()
	root := t.TempDir()
	full := filepath.Join(root, ".devcontainer", "devcontainer.json")
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return &inspector.Repo{Root: root}
}
