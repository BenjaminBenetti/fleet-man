// Package inspector provides a generic, plugin-friendly mechanism for
// looking at a fleet's remote repository without ever provisioning an
// instance. Open clones the remote into a temporary directory and
// returns a *Repo handle that individual checks (under
// internal/inspector/check/...) can use to read config files, detect
// tooling, or otherwise reason about the project.
//
// A typical usage looks like:
//
//	repo, err := inspector.Open(remoteURL, branch)
//	if err != nil { return err }
//	defer repo.Close()
//
//	homeDir, err := homedir.Detect(repo)
//	// ... future checks operate on the same repo handle ...
//
// Cloning happens once per Open call; checks are cheap to run against
// the resulting Repo.
package inspector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// ===========================================
// Errors
// ===========================================

// ErrNoDevcontainerConfig is returned by FindDevcontainerJSON when the
// repository contains no devcontainer.json in any conventional
// location. Callers can compare with errors.Is to distinguish "not a
// devcontainer repo" from other read failures.
var ErrNoDevcontainerConfig = errors.New("no devcontainer.json found")

// ===========================================
// Public types
// ===========================================

// Repo is a handle to a shallow-cloned local copy of a fleet's remote
// repository — or, for a file:// template remote, to the template
// directory itself. Checks read from Root either directly (path is a
// normal filesystem location) or via the convenience helpers on this type.
//
// A Repo MUST be closed via Close to remove its on-disk clone. Close is a
// no-op for a template Repo (Keep): Root is the user's own directory.
type Repo struct {
	// Root is the absolute path to the working tree of the clone.
	// Checks may read any file under this path.
	Root string

	// Keep marks Root as a directory this handle does NOT own — a file://
	// template dir opened in place — so Close leaves it alone. Zero (a
	// scratch clone, or a test-built Repo) keeps the remove-on-Close contract.
	Keep bool
}

// ===========================================
// Lifecycle
// ===========================================

// Open shallow-clones remoteURL (optionally at branch) into a temporary
// directory and returns a Repo handle. The clone uses --depth 1 so the
// network cost is roughly proportional to the size of HEAD; the
// caller-bound 90-second timeout keeps a hung clone from blocking the
// surrounding workflow forever.
//
// The temporary directory is removed by Close. If Open itself returns
// an error the temp dir is cleaned up before returning, so callers
// only need to defer Close on the success path.
func Open(remoteURL, branch string) (*Repo, error) {
	if remoteURL == "" {
		return nil, errors.New("remoteURL is empty")
	}
	if fleet.IsTemplateRemote(remoteURL) {
		return openTemplate(remoteURL)
	}
	tmpDir, err := os.MkdirTemp("", "fleet-inspect-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	if err := shallowClone(remoteURL, branch, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("clone: %w", err)
	}
	return &Repo{Root: tmpDir}, nil
}

// openTemplate points a Repo at a file:// template directory in place. There
// is nothing to clone: the checks read the directory that provisioning will
// copy, and the branch (if any) is ignored because a copy has no refs.
func openTemplate(remoteURL string) (*Repo, error) {
	dir, err := fleet.TemplateDir(remoteURL)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("template dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("template path %s is not a directory", dir)
	}
	return &Repo{Root: dir, Keep: true}, nil
}

// Close removes the temporary clone associated with this Repo. Safe to
// call on a nil receiver or a zero-value Repo. A Keep Repo (a template dir,
// the user's own directory rather than a scratch clone) is left untouched.
func (r *Repo) Close() error {
	if r == nil || r.Root == "" || r.Keep {
		return nil
	}
	return os.RemoveAll(r.Root)
}

// ===========================================
// Devcontainer helpers
// ===========================================

// FindDevcontainerJSON locates the first devcontainer.json in the repo
// using the same search order as the devcontainer CLI: the canonical
// `.devcontainer/devcontainer.json` is tried first, then the
// repository-root variant `.devcontainer.json`, then any subfolder
// under `.devcontainer/` (the multi-config layout used by some
// monorepos).
//
// Returns the absolute path and contents on success, or
// ErrNoDevcontainerConfig when no candidate exists.
func (r *Repo) FindDevcontainerJSON() (string, []byte, error) {
	candidates := []string{
		filepath.Join(r.Root, ".devcontainer", "devcontainer.json"),
		filepath.Join(r.Root, ".devcontainer.json"),
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			return candidate, data, nil
		}
	}

	devcontainerDir := filepath.Join(r.Root, ".devcontainer")
	entries, err := os.ReadDir(devcontainerDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(devcontainerDir, entry.Name(), "devcontainer.json")
			if data, err := os.ReadFile(candidate); err == nil {
				return candidate, data, nil
			}
		}
	}

	return "", nil, ErrNoDevcontainerConfig
}

// ===========================================
// Internal helpers
// ===========================================

// shallowClone runs `git clone --depth 1` into dest. A 90-second
// context bounds the clone so an unreachable host eventually surfaces
// as an error rather than hanging the dialog that triggered Open.
func shallowClone(remoteURL, branch, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	args := []string{"clone", "--depth", "1", "--no-tags"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, remoteURL, dest)

	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
