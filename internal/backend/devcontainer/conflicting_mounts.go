package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// neutralizeConflictingMounts ensures that any mount entries in the
// workspace's devcontainer.json whose container target collides with a
// fleet-supplied mount are removed for the duration of `devcontainer
// up`. Without this, the devcontainer CLI passes both the user's mount
// and fleet's mount to `docker run`, which rejects the command with
// "Duplicate mount point".
//
// fleet's mounts win: the user's conflicting entries are stripped out
// of the on-disk file before Up runs. The original file contents are
// captured up front, and the returned restore func rewrites them back
// verbatim so the workspace (which is a real git working tree the user
// will commit from) is left exactly as it was found.
//
// A no-op restore is returned when there is no devcontainer.json, when
// the file declares no conflicting mounts, or when parsing fails — in
// those cases the file is never touched. Callers should defer the
// restore unconditionally.
func neutralizeConflictingMounts(workspaceDir string, fleetMounts []backend.Mount) (func(), error) {
	noop := func() {}
	if len(fleetMounts) == 0 {
		return noop, nil
	}

	configPath, original, err := findDevcontainerJSON(workspaceDir)
	if err != nil {
		// No config to fight with: nothing to do.
		if errors.Is(err, os.ErrNotExist) {
			return noop, nil
		}
		return noop, err
	}

	rewritten, conflicts, err := stripConflictingMounts(original, fleetMounts)
	if err != nil {
		// Parse failures are non-fatal: leave the file alone and let
		// devcontainer up surface its own error if there really is a
		// conflict. Erroring here would block instance creation for
		// users with valid-but-exotic JSONC the CLI accepts. But log
		// to stderr so the failure mode is visible — silently
		// skipping is what previously hid a regex bug.
		fmt.Fprintf(os.Stderr, "fleet: could not parse %s to neutralise conflicting mounts (continuing anyway): %v\n", configPath, err)
		return noop, nil
	}
	if len(conflicts) == 0 {
		return noop, nil
	}

	if err := os.WriteFile(configPath, rewritten, 0644); err != nil {
		return noop, fmt.Errorf("rewriting %s: %w", configPath, err)
	}

	fmt.Fprintf(os.Stderr, "fleet: overriding %d mount(s) in %s: %s\n", len(conflicts), configPath, strings.Join(conflicts, ", "))

	restore := func() {
		_ = os.WriteFile(configPath, original, 0644)
	}
	return restore, nil
}

// findDevcontainerJSON returns the path and raw bytes of the
// devcontainer.json that `devcontainer up --workspace-folder
// workspaceDir` will use. Search order matches the CLI: the canonical
// .devcontainer/devcontainer.json first, then the repo-root
// .devcontainer.json. The subfolder multi-config layout requires the
// caller to pass --config, which fleet does not do, so it is not
// covered here.
func findDevcontainerJSON(workspaceDir string) (string, []byte, error) {
	candidates := []string{
		filepath.Join(workspaceDir, ".devcontainer", "devcontainer.json"),
		filepath.Join(workspaceDir, ".devcontainer.json"),
	}
	for _, candidate := range candidates {
		configBytes, err := os.ReadFile(candidate)
		if err == nil {
			return candidate, configBytes, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
	}
	return "", nil, os.ErrNotExist
}

// stripConflictingMounts removes every mount entry in original whose
// container target equals the ContainerPath of one of fleetMounts.
// Returns the rewritten bytes, the list of removed target paths (so
// callers can report what was overridden), and an error only when the
// file cannot be parsed as JSONC.
//
// The rewrite goes through json.Marshal, which loses comments and
// original formatting. That is acceptable because the caller restores
// the original bytes once `devcontainer up` returns — the rewritten
// file only lives on disk for the duration of that call.
func stripConflictingMounts(original []byte, fleetMounts []backend.Mount) ([]byte, []string, error) {
	conflictingTargets := make(map[string]struct{}, len(fleetMounts))
	for _, mount := range fleetMounts {
		if mount.ContainerPath == "" {
			continue
		}
		conflictingTargets[mount.ContainerPath] = struct{}{}
	}
	if len(conflictingTargets) == 0 {
		return original, nil, nil
	}

	var config map[string]any
	if err := json.Unmarshal(toStrictJSON(original), &config); err != nil {
		return nil, nil, err
	}
	rawMounts, ok := config["mounts"].([]any)
	if !ok {
		return original, nil, nil
	}

	var removed []string
	kept := make([]any, 0, len(rawMounts))
	for _, entry := range rawMounts {
		target := mountEntryTarget(entry)
		if _, conflict := conflictingTargets[target]; conflict {
			removed = append(removed, target)
			continue
		}
		kept = append(kept, entry)
	}
	if len(removed) == 0 {
		return original, nil, nil
	}

	if len(kept) == 0 {
		delete(config, "mounts")
	} else {
		config["mounts"] = kept
	}

	rewritten, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return rewritten, removed, nil
}

// mountEntryTarget extracts the container target path from one element
// of the devcontainer.json `mounts` array. Two shapes are recognised:
//
//   - string form: "source=...,target=...,type=bind" — the
//     comma-separated key=value syntax accepted by docker run --mount.
//     `target`, `dst`, and `destination` are all valid spellings for
//     the destination path.
//   - object form: {"target": "...", "source": "...", "type": "bind"}.
//     `destination` is also accepted as an alias for `target`.
//
// Returns an empty string when the entry shape is unrecognised so the
// caller leaves it alone rather than guessing.
func mountEntryTarget(entry any) string {
	switch value := entry.(type) {
	case string:
		for part := range strings.SplitSeq(value, ",") {
			keyVal := strings.SplitN(part, "=", 2)
			if len(keyVal) != 2 {
				continue
			}
			switch strings.TrimSpace(keyVal[0]) {
			case "target", "dst", "destination":
				return strings.TrimSpace(keyVal[1])
			}
		}
	case map[string]any:
		for _, key := range []string{"target", "destination"} {
			if target, ok := value[key].(string); ok {
				return target
			}
		}
	}
	return ""
}

// toStrictJSON converts a JSONC payload (the dialect devcontainer.json
// uses — line comments, block comments, trailing commas) into strict
// JSON that encoding/json can parse.
//
// Implemented as two string-aware passes rather than a regex pipeline
// because real devcontainer.json files routinely contain URL values
// like "http://localhost:81/" inside `customizations`, port
// attributes, and so on. A naive `//[^\n]*` regex would strip the
// rest of those lines, producing invalid JSON and a silent no-op
// upstream — that is exactly how the original implementation of this
// function masked a real conflict.
//
// Pass 1 strips comments; pass 2 elides trailing commas. The two
// passes are kept separate so a comma followed by a line comment
// followed by `]` (which only looks like a trailing comma AFTER pass
// 1 has removed the comment) is correctly handled. Both passes track
// string state so any `//`, `/*`, or `,` byte inside a string literal
// is preserved verbatim.
func toStrictJSON(jsoncBytes []byte) []byte {
	return stripTrailingCommas(stripJSONCComments(jsoncBytes))
}

// stripJSONCComments removes // line comments and /* block comments
// from a JSONC payload, leaving comment markers inside string
// literals untouched. Line endings of stripped line comments are
// preserved so error line numbers stay aligned with the original
// file; block-comment line counts may shift.
func stripJSONCComments(jsoncBytes []byte) []byte {
	out := make([]byte, 0, len(jsoncBytes))
	inString := false
	escaped := false

	for i := 0; i < len(jsoncBytes); i++ {
		current := jsoncBytes[i]

		if inString {
			out = append(out, current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}

		if current == '"' {
			inString = true
			out = append(out, current)
			continue
		}

		if current == '/' && i+1 < len(jsoncBytes) && jsoncBytes[i+1] == '/' {
			for i < len(jsoncBytes) && jsoncBytes[i] != '\n' {
				i++
			}
			if i < len(jsoncBytes) {
				out = append(out, jsoncBytes[i])
			}
			continue
		}

		if current == '/' && i+1 < len(jsoncBytes) && jsoncBytes[i+1] == '*' {
			i += 2
			for i+1 < len(jsoncBytes) && !(jsoncBytes[i] == '*' && jsoncBytes[i+1] == '/') {
				i++
			}
			i++ // step past the closing '/'
			continue
		}

		out = append(out, current)
	}
	return out
}

// stripTrailingCommas drops every comma that is followed only by
// whitespace and a closing `]` or `}`, anywhere outside a string
// literal. Commas inside string values are preserved verbatim — a
// blind regex `,(\s*[}\]])` would silently corrupt strings like
// "foo,]" otherwise.
func stripTrailingCommas(jsonBytes []byte) []byte {
	out := make([]byte, 0, len(jsonBytes))
	inString := false
	escaped := false

	for i := 0; i < len(jsonBytes); i++ {
		current := jsonBytes[i]

		if inString {
			out = append(out, current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}

		if current == '"' {
			inString = true
			out = append(out, current)
			continue
		}

		if current == ',' {
			lookahead := i + 1
			for lookahead < len(jsonBytes) && isJSONWhitespace(jsonBytes[lookahead]) {
				lookahead++
			}
			if lookahead < len(jsonBytes) && (jsonBytes[lookahead] == ']' || jsonBytes[lookahead] == '}') {
				continue
			}
		}

		out = append(out, current)
	}
	return out
}

// isJSONWhitespace reports whether b is one of the four whitespace
// bytes recognised by JSON (space, tab, newline, carriage return).
func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
