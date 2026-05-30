package agentdetect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ===========================================
// Claude Code settings.json injection
// ===========================================
//
// fleet-man detects Claude Code's run state by installing hooks into the
// container's ~/.claude/settings.json that fire on lifecycle events
// (UserPromptSubmit, PreToolUse, PostToolUse, Notification, Stop) and
// write a single-line state file the host then reads.
//
// settings.json is a user file that may contain unrelated keys, the
// user's own hooks, plugin hooks, organization policy, or future fields
// fleet-man has never heard of. The reconciliation logic in this file
// is therefore strictly EDIT-IN-PLACE:
//
//   - The document is parsed into map[string]any, never into a typed
//     struct, so unknown fields round-trip untouched.
//   - We identify our own hook entries solely by the command path of
//     the inner hook (FleetManHookCommand). Anything not bearing that
//     marker is foreign and is left exactly as we found it.
//   - Every parent path is created lazily (the file may be missing,
//     empty, contain only "{}", contain "hooks": null, etc.).
//   - The function is a pure transformation on bytes; the caller does
//     the atomic write.
//
// Hook entries fleet-man manages have this canonical shape:
//
//   {
//     "matcher": "*",
//     "hooks": [
//       {"type": "command", "command": "<FleetManHookCommand> <Event>"}
//     ]
//   }

// ===========================================
// Constants
// ===========================================

// FleetManScriptSuffix is the home-relative path of the state-writing
// hook script. The absolute path is `$HOME/<suffix>` and varies per
// container (different remoteUser → different $HOME), so settings
// reconciliation receives the absolute path as a parameter rather
// than reading it from a constant.
//
// Suffix-based recognition (rather than full-path equality) lets us
// identify "our" entries without knowing the absolute path:
// installations whose home dir differs from ours still produce a
// command that ends in this suffix, so a fleet-man entry written by
// any version is still recognisably ours and gets updated rather
// than duplicated.
const FleetManScriptSuffix = ".fleet/scripts/claude-state-hook.sh"

// fleetManManagedEvents lists the Claude Code lifecycle events
// fleet-man installs handlers for. Order here governs the order in
// which our entries are first inserted; subsequent reconciliations
// preserve whatever position our entry already occupies in each
// event's array.
var fleetManManagedEvents = []string{
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"Notification",
	"Stop",
}

// ===========================================
// Public API
// ===========================================

// InjectFleetManHooks upserts fleet-man's state-writing hook entries
// into a Claude Code settings.json document, returning the new bytes.
//
// current is the current contents of settings.json. nil, an empty
// slice, or whitespace-only input is treated as "the file does not
// exist yet" and yields a fresh document containing only our hooks.
//
// scriptPath is the absolute path of the in-container hook script.
// The provisioner computes this from the container's $HOME plus
// FleetManScriptSuffix. Tests pass any path ending in the suffix.
//
// Guarantees:
//
//   - Every key, value, and hook entry that fleet-man does not own is
//     preserved exactly as it appeared in the input. Ownership is
//     determined by suffix-matching the command path against
//     FleetManScriptSuffix, so any prior fleet-man entry — even one
//     written under a different $HOME — is recognised and updated.
//   - Idempotent: running the function on its own output produces the
//     same output.
//   - Position-stable: when our entry already exists in an event's
//     array we update it in place rather than moving it.
//   - Never writes to disk. The caller is responsible for atomic
//     write semantics.
//
// Returns an error only when current is non-empty and is not valid
// JSON, or when the parsed structure conflicts with the schema in a
// way we cannot safely repair (for example "hooks" is an array
// instead of an object). In every error case the original bytes are
// safe to leave on disk untouched.
func InjectFleetManHooks(current []byte, scriptPath string) ([]byte, error) {
	root := map[string]any{}
	trimmed := bytes.TrimSpace(current)
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &root); err != nil {
			return nil, fmt.Errorf("parse settings.json: %w", err)
		}
	}

	hooks, err := getOrCreateHooksMap(root)
	if err != nil {
		return nil, err
	}

	for _, event := range fleetManManagedEvents {
		if err := upsertEvent(hooks, event, scriptPath); err != nil {
			return nil, err
		}
	}

	root["hooks"] = hooks

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal settings.json: %w", err)
	}
	return append(out, '\n'), nil
}

// ===========================================
// Private Helpers
// ===========================================

// getOrCreateHooksMap returns the "hooks" object from root, creating
// an empty one if the key is absent or explicitly null. Returns an
// error when "hooks" is present with an incompatible type — we never
// silently overwrite a user value of unexpected shape.
func getOrCreateHooksMap(root map[string]any) (map[string]any, error) {
	raw, present := root["hooks"]
	if !present || raw == nil {
		return map[string]any{}, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`"hooks" must be a JSON object, got %T`, raw)
	}
	return hooks, nil
}

// upsertEvent reconciles fleet-man's matcher group inside the array
// at hooks[event].
//
// Behaviour:
//   - If our group is present, it is replaced in place (preserving
//     its position relative to foreign entries).
//   - Any duplicate fleet-man groups are collapsed into a single
//     entry at the position of the first occurrence.
//   - If our group is absent, it is appended after all existing
//     entries.
//   - Foreign matcher groups are passed through unchanged.
//
// Returns an error if hooks[event] exists with a non-array type.
func upsertEvent(hooks map[string]any, event, scriptPath string) error {
	raw, present := hooks[event]
	var groups []any
	if present && raw != nil {
		existing, ok := raw.([]any)
		if !ok {
			return fmt.Errorf(`hooks[%q] must be a JSON array, got %T`, event, raw)
		}
		groups = existing
	}

	desired := desiredGroup(event, scriptPath)

	result := make([]any, 0, len(groups)+1)
	foundOurs := false
	for _, group := range groups {
		if isFleetManGroup(group) {
			if !foundOurs {
				result = append(result, desired)
				foundOurs = true
			}
			continue
		}
		result = append(result, group)
	}
	if !foundOurs {
		result = append(result, desired)
	}
	hooks[event] = result
	return nil
}

// desiredGroup returns the canonical fleet-man matcher group for the
// given event. Building a fresh map every call means later mutations
// (during reconciliation of subsequent events, or by the caller) can
// never bleed back into shared state.
func desiredGroup(event, scriptPath string) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": scriptPath + " " + event,
			},
		},
	}
}

// isFleetManGroup reports whether the given matcher-group entry
// belongs to fleet-man, identified by the command path of any of
// its inner hooks. Robust to malformed entries: anything that does
// not match the expected shape simply returns false (treat as
// foreign, leave alone).
func isFleetManGroup(group any) bool {
	groupMap, ok := group.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := groupMap["hooks"].([]any)
	if !ok {
		return false
	}
	for _, hookEntry := range inner {
		hookMap, ok := hookEntry.(map[string]any)
		if !ok {
			continue
		}
		command, ok := hookMap["command"].(string)
		if !ok {
			continue
		}
		if isFleetManCommand(command) {
			return true
		}
	}
	return false
}

// isFleetManCommand reports whether a hook command string was
// installed by fleet-man. Matching by the home-relative
// FleetManScriptSuffix (rather than a full absolute path) means
// entries written by an installation under a different $HOME — for
// example a different remoteUser between fleet-man versions or
// re-provisioning passes — are still recognised as ours and get
// updated rather than duplicated.
func isFleetManCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	first := command
	if idx := strings.IndexAny(command, " \t"); idx >= 0 {
		first = command[:idx]
	}
	return strings.HasSuffix(first, "/"+FleetManScriptSuffix)
}
