package fleet

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Custom mounts let a user attach an arbitrary number of shared directories to
// a fleet. The user supplies only the in-container target path; the host side
// is always derived as ~/.fleet/workspaces/<fleet>/.mnt/<path> by the resolver.
// Because that user input becomes a host filesystem path segment, the input is
// validated here — at the domain layer — so both the TUI (for immediate UX
// feedback) and the server (authoritatively, in SetFleetSettings) share one
// definition of what a legal mount path is.

// NormalizeCustomMount validates a single user-supplied custom-mount container
// path and returns its canonical form. The rules:
//
//   - leading/trailing whitespace is trimmed;
//   - the path must be non-empty;
//   - the path must be ABSOLUTE (start with "/") — this keeps the derived host
//     subdirectory unambiguous and matches the documented contract;
//   - no path segment may be ".." — this is the path-traversal boundary: the
//     derived host path (filepath.Join(".mnt", <path>)) must never escape the
//     fleet's .mnt directory;
//   - the cleaned path must not be the container root "/" (mounting a shared
//     directory over / is never what the user wants and would shadow the whole
//     filesystem).
//
// The returned string is filepath.Clean'd so equivalent spellings (trailing
// slash, "/a//b", "/a/./b") collapse to one canonical value, which also makes
// deduplication meaningful.
func NormalizeCustomMount(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("mount path is empty")
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("mount path %q must be absolute (start with '/')", trimmed)
	}
	// Reject explicit ".." segments before cleaning so the error is clear; a
	// later filepath.Clean would silently resolve them, hiding the intent.
	if slices.Contains(strings.Split(trimmed, "/"), "..") {
		return "", fmt.Errorf("mount path %q must not contain '..'", trimmed)
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == "/" {
		return "", fmt.Errorf("mount path must not be the container root '/'")
	}
	return cleaned, nil
}

// NormalizeCustomMountHostFolder validates a custom mount's host folder: a
// path on the daemon host, absolute and cleaned, never the host's root, and
// free of what docker's --mount option could not carry in its
// comma-separated value (a comma, a double quote, a control character).
// Whether it exists is the daemon host's to say when an instance is made.
func NormalizeCustomMountHostFolder(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("host folder is empty")
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("host folder %q must be absolute (start with '/')", trimmed)
	}
	if strings.ContainsAny(trimmed, ",\"") || strings.ContainsFunc(trimmed, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("host folder %q must not contain a comma, a double quote or a control character", trimmed)
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == "/" {
		return "", fmt.Errorf("host folder must not be the root '/'")
	}
	return cleaned, nil
}

// NormalizeCustomMountHostFolders validates the host folders of a fleet's
// custom mounts (mounts, already normalized) and returns them keyed by each
// mount's normalized path. An entry with an empty folder means the mount
// keeps its .mnt folder and is dropped, as is one for a path that is no
// custom mount (the mount was removed); an empty result is nil. The first
// invalid folder stops it.
func NormalizeCustomMountHostFolders(mounts []string, folders map[string]string) (map[string]string, error) {
	var out map[string]string
	for raw, folder := range folders {
		if strings.TrimSpace(folder) == "" {
			continue
		}
		path, err := NormalizeCustomMount(raw)
		if err != nil || !slices.Contains(mounts, path) {
			continue
		}
		norm, err := NormalizeCustomMountHostFolder(folder)
		if err != nil {
			return nil, fmt.Errorf("custom mount %s: %w", path, err)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[path] = norm
	}
	return out, nil
}

// SetCustomMount adds the custom mount at path to s (normalized, after the
// others) or, when s has it, keeps it where it is, and gives it host
// folder ("" keeps it on its .mnt folder). It returns the changed settings.
func SetCustomMount(s FleetSettings, path, hostFolder string) (FleetSettings, error) {
	norm, err := NormalizeCustomMount(path)
	if err != nil {
		return s, err
	}
	if !slices.Contains(s.CustomMounts, norm) {
		s.CustomMounts = append(slices.Clone(s.CustomMounts), norm)
	}
	folders := maps.Clone(s.CustomMountHostFolders)
	if hostFolder == "" {
		delete(folders, norm)
	} else {
		folder, err := NormalizeCustomMountHostFolder(hostFolder)
		if err != nil {
			return s, fmt.Errorf("custom mount %s: %w", norm, err)
		}
		if folders == nil {
			folders = map[string]string{}
		}
		folders[norm] = folder
	}
	if len(folders) == 0 {
		folders = nil
	}
	s.CustomMountHostFolders = folders
	return s, nil
}

// RemoveCustomMount drops the custom mount at path from s, with its host
// folder. A path s does not mount is an error.
func RemoveCustomMount(s FleetSettings, path string) (FleetSettings, error) {
	norm, err := NormalizeCustomMount(path)
	if err != nil {
		return s, err
	}
	i := slices.Index(s.CustomMounts, norm)
	if i < 0 {
		return s, fmt.Errorf("no custom mount at %s", norm)
	}
	s.CustomMounts = slices.Delete(slices.Clone(s.CustomMounts), i, i+1)
	if len(s.CustomMounts) == 0 {
		s.CustomMounts = nil
	}
	folders := maps.Clone(s.CustomMountHostFolders)
	delete(folders, norm)
	if len(folders) == 0 {
		folders = nil
	}
	s.CustomMountHostFolders = folders
	return s, nil
}

// NormalizeCustomMounts validates every entry of in and returns the normalized,
// de-duplicated list (input order preserved, first occurrence kept). An empty
// or nil input yields a nil slice and no error. The first invalid entry stops
// validation and its error is returned, so callers can surface a precise
// message and reject the whole update.
//
// Note this does NOT reject a custom mount that collides with a managed mount
// target (Claude/Codex/Gh): by design such a path is allowed and resolves
// last-wins. It only de-duplicates exact repeats of the same custom path, which
// would otherwise be redundant no-ops.
func NormalizeCustomMounts(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var out []string
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		norm, err := NormalizeCustomMount(raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}
