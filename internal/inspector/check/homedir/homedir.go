// Package homedir is an inspector check: given an *inspector.Repo it
// guesses the home directory of the user that the fleet's container
// will run as. The result is used to anchor agentic mounts (Claude
// Code, Codex, …) under the right location inside the container.
//
// The check is best-effort by design. Real-world devcontainer.json
// files use JSONC (line comments, trailing commas), and the USER
// directive of the underlying image is only reachable when docker is
// installed and the image is locally pulled. When any step fails the
// caller should fall back to letting the user type a value manually.
package homedir

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

// ===========================================
// Errors
// ===========================================

// ErrNoUserHint is returned when the devcontainer config and (if
// reachable) the underlying image both fail to indicate which user
// the container will run as.
var ErrNoUserHint = errors.New("could not determine container user")

// ===========================================
// Public API
// ===========================================

// Detect derives the container's home directory from repo's
// devcontainer config in priority order:
//
//  1. an explicit "remoteUser"/"containerUser" field in
//     devcontainer.json;
//  2. the USER directive of the "image" referenced by that file
//     (requires `docker` on PATH; the image is pulled if necessary).
//
// Returns the absolute path (e.g. "/home/vscode" or "/root"). When
// neither signal is available, ErrNoUserHint is returned and the
// caller should let the user enter a value by hand.
func Detect(repo *inspector.Repo) (string, error) {
	_, contents, err := repo.FindDevcontainerJSON()
	if err != nil {
		return "", err
	}

	// Strip JSONC comments before pattern matching. Without this, a
	// commented-out `"remoteUser": "root"` line would be picked up
	// alongside (or before) the live setting beneath it.
	contents = stripJSONCComments(contents)

	if user := extractUserFromConfig(contents); user != "" {
		return homeForUser(user), nil
	}

	if image := extractImageFromConfig(contents); image != "" {
		if user, err := userFromImage(image); err == nil && user != "" {
			return homeForUser(user), nil
		}
	}

	return "", ErrNoUserHint
}

// ===========================================
// Field extraction
// ===========================================

// blockCommentRegex matches /* … */ comments. The (?s) flag lets `.`
// span newlines, and `.*?` is non-greedy so two adjacent block
// comments are not collapsed into one.
var blockCommentRegex = regexp.MustCompile(`(?s)/\*.*?\*/`)

// lineCommentRegex matches // … to end of line.
var lineCommentRegex = regexp.MustCompile(`//[^\n]*`)

// stripJSONCComments removes line and block comments from a JSONC
// payload so the field-extraction regexes below cannot match patterns
// living inside a comment. The implementation is intentionally simple:
// it does not skip over `//` or `/*` that happen to appear inside a
// string literal, because the only fields this package looks for
// (remoteUser, containerUser, image) hold values that never contain
// those sequences in practice.
func stripJSONCComments(configBytes []byte) []byte {
	configBytes = blockCommentRegex.ReplaceAll(configBytes, nil)
	configBytes = lineCommentRegex.ReplaceAll(configBytes, nil)
	return configBytes
}

// remoteUserRegex matches a "remoteUser" or "containerUser" field in
// devcontainer.json. Callers should pass bytes that have already been
// run through stripJSONCComments so a commented-out occurrence does
// not shadow the live one.
var remoteUserRegex = regexp.MustCompile(`"(?:remoteUser|containerUser)"\s*:\s*"([^"]+)"`)

// imageRegex matches the "image" field in devcontainer.json. Same
// stripJSONCComments expectation as remoteUserRegex.
var imageRegex = regexp.MustCompile(`"image"\s*:\s*"([^"]+)"`)

// extractUserFromConfig returns the user name found in either
// remoteUser or containerUser, preferring whichever appears first.
// Returns an empty string if neither is set.
func extractUserFromConfig(configBytes []byte) string {
	match := remoteUserRegex.FindSubmatch(configBytes)
	if len(match) < 2 {
		return ""
	}
	return string(match[1])
}

// extractImageFromConfig returns the image reference declared in
// devcontainer.json, or an empty string when the file uses a
// Dockerfile or compose-based build instead.
func extractImageFromConfig(configBytes []byte) string {
	match := imageRegex.FindSubmatch(configBytes)
	if len(match) < 2 {
		return ""
	}
	return string(match[1])
}

// ===========================================
// Image inspection
// ===========================================

// userFromImage returns the highest-precedence user the image hints
// at, in this order: (1) the last remoteUser/containerUser declared
// in the image's `devcontainer.metadata` label — this is what the
// devcontainer CLI itself uses, and is the only place Microsoft's
// base images stash their default user; (2) the image's USER
// directive (Config.User). If the image is not present locally we
// run `docker pull` first.
//
// Returns an empty string with no error when the image was inspected
// successfully but advertised no user (Docker's default — root).
func userFromImage(image string) (string, error) {
	user, err := bestUserFromInspect(image)
	if err == nil {
		return user, nil
	}
	if pullErr := dockerPull(image); pullErr != nil {
		return "", pullErr
	}
	return bestUserFromInspect(image)
}

// bestUserFromInspect returns the metadata-label user when one is
// declared, falling back to Config.User otherwise. An error is
// returned only when docker itself can't inspect the image (typically
// "image not found locally"), used as a signal to pull and retry.
func bestUserFromInspect(image string) (string, error) {
	if user, err := dockerInspectMetadataUser(image); err == nil && user != "" {
		return user, nil
	}
	return dockerInspectUser(image)
}

// dockerInspectUser returns the User directive baked into image's
// config. An empty string means the image was inspected successfully
// but ran as root (Docker's default).
func dockerInspectUser(image string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Config.User}}", image)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	user := strings.TrimSpace(string(out))
	// docker emits "<no value>" when User is unset; treat that as root.
	if user == "<no value>" {
		return "", nil
	}
	return user, nil
}

// dockerInspectMetadataUser reads the image's "devcontainer.metadata"
// label and returns the last remoteUser / containerUser declared in
// it. The label is a JSON array of feature/config objects; the
// devcontainer spec composes them in order with later entries
// overriding earlier ones, so the last user wins.
//
// An empty string with no error means the image was inspected
// successfully but had no label or no user directive in it. An error
// is returned only when docker itself fails (typically the image is
// not present locally).
func dockerInspectMetadataUser(image string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", `{{index .Config.Labels "devcontainer.metadata"}}`, image)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	label := strings.TrimSpace(string(out))
	// docker emits "<no value>" when the label is absent.
	if label == "" || label == "<no value>" {
		return "", nil
	}
	return lastUserInMetadata([]byte(label)), nil
}

// lastUserInMetadata extracts the last remoteUser/containerUser value
// in a devcontainer.metadata label payload. Returns an empty string
// when none is declared.
func lastUserInMetadata(label []byte) string {
	matches := remoteUserRegex.FindAllSubmatch(label, -1)
	if len(matches) == 0 {
		return ""
	}
	return string(matches[len(matches)-1][1])
}

// dockerPull pulls image into the local docker daemon's cache.
func dockerPull(image string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ===========================================
// User → home dir mapping
// ===========================================

// homeForUser maps a container user name to the conventional home
// directory path. The Docker default — root — gets /root; everyone
// else gets /home/<user>. Images that override HOME via ENV will be
// wrong here, but those are rare enough that the user can correct the
// detector's output by hand.
//
// User specifications of the form "uid:gid" or "name:group" are
// accepted; only the part before the colon is used.
func homeForUser(user string) string {
	if i := strings.IndexByte(user, ':'); i >= 0 {
		user = user[:i]
	}
	user = strings.TrimSpace(user)
	if user == "" || user == "root" || user == "0" {
		return "/root"
	}
	return "/home/" + user
}
