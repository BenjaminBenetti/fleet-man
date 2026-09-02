package fleet

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// TemplateScheme is the URL scheme that marks a fleet remote as a local
// TEMPLATE DIRECTORY rather than a git remote. A template fleet does not
// clone anything: the directory is copied verbatim (cp -a) into each new
// instance's workspace, so one-off repos and scratch projects can be fleeted
// without pushing them anywhere first.
//
// The path is resolved on the daemon host (the machine that provisions), so a
// remote TUI must name a directory that exists THERE, not on the client.
const TemplateScheme = "file://"

// IsTemplateRemote reports whether remote names a local template directory
// (file:///abs/path) instead of a git remote to clone.
func IsTemplateRemote(remote string) bool {
	return strings.HasPrefix(strings.TrimSpace(remote), TemplateScheme)
}

// TemplateDir returns the cleaned absolute directory a file:// remote points
// at. Only the file:///abs/path and file://localhost/abs/path forms are
// accepted: a relative path has no stable meaning once the daemon (not the
// user's shell) resolves it, and any other host would silently name a
// directory on the wrong machine. Percent-escapes are decoded so a URL pasted
// from a file manager (%20 for a space) resolves to the real path.
func TemplateDir(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if !IsTemplateRemote(remote) {
		return "", fmt.Errorf("%q is not a %s URL", remote, TemplateScheme)
	}
	rest := strings.TrimPrefix(remote, TemplateScheme)
	if strings.HasPrefix(rest, "localhost/") {
		rest = strings.TrimPrefix(rest, "localhost")
	}
	if !strings.HasPrefix(rest, "/") {
		// file://host/path or file://relative/path: either way the caller
		// left out the third slash that makes the path absolute.
		return "", fmt.Errorf("template path must be absolute: use %s/abs/path (got %q)", TemplateScheme, remote)
	}
	if decoded, err := url.PathUnescape(rest); err == nil {
		rest = decoded
	}
	dir := filepath.Clean(rest)
	if dir == "/" {
		return "", fmt.Errorf("template path must name a directory, not the filesystem root")
	}
	return dir, nil
}

// templateHostHint reminds a remote-TUI user that the path is checked on the
// daemon's machine, not theirs. Every ResolveTemplateDir caller (job-start
// validation, InspectRepo, the workspace copy) runs on the daemon host, so the
// hint is accurate wherever the error surfaces.
const templateHostHint = " (the path is resolved on the fleet daemon's host)"

// ResolveTemplateDir is TemplateDir plus the existence check: the directory
// must exist (on THIS host — the daemon's) and be a directory. Every consumer
// of a template path goes through here so the rule and its wording cannot
// drift between them.
func ResolveTemplateDir(remote string) (string, error) {
	dir, err := TemplateDir(remote)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("template dir: %w%s", err, templateHostHint)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("template path %s is not a directory%s", dir, templateHostHint)
	}
	return dir, nil
}

// TemplateNameHint suggests a fleet name for a template remote: the base name
// of the template directory. It is only a DEFAULT for the user to confirm or
// edit — unlike a git URL, a local path carries no authoritative project name,
// which is why FleetNameFromRemote refuses to derive one automatically.
func TemplateNameHint(remote string) string {
	dir, err := TemplateDir(remote)
	if err != nil {
		return ""
	}
	return filepath.Base(dir)
}

// ValidateTemplateCreate checks the create-instance inputs that only make
// sense for a git remote against a template remote. It is a no-op for git
// remotes. Both the server (at job start, so `fleet up` fails fast) and
// create.Run (the enforcement point) call it, so the rule has one home.
func ValidateTemplateCreate(remote, branch string, backendType BackendType) error {
	if !IsTemplateRemote(remote) {
		return nil
	}
	if _, err := TemplateDir(remote); err != nil {
		return err
	}
	if branch != "" {
		return fmt.Errorf("a branch cannot be checked out for a %s template fleet (the template dir is copied as-is)", TemplateScheme)
	}
	if backendType != BackendDevcontainer {
		return fmt.Errorf("a %s template fleet requires the devcontainer backend (got %s)", TemplateScheme, backendType)
	}
	return nil
}
