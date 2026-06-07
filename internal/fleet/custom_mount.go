package fleet

import (
	"fmt"
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
