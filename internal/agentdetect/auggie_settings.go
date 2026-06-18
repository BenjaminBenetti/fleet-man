package agentdetect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ===========================================
// Auggie (Augment CLI) settings.json injection
// ===========================================
//
// fleet-man detects auggie's run state by installing hooks into the
// container's ~/.augment/settings.json that fire on lifecycle events
// (SessionStart, PromptSubmit, PreToolUse, PostToolUse, Notification,
// Stop) and write a single-line state file the host then reads.
//
// The reconciliation rules mirror claude_settings.go exactly — strict
// edit-in-place on an untyped map so unrelated keys round-trip, with
// fleet-man's own entries recognised solely by the command path
// (AuggieScriptSuffix). Two details differ from Claude's settings,
// both dictated by auggie's native schema:
//
//   - Matcher: auggie matches a hook group against the tool name by
//     treating "matcher" as a REGEX (default ".*"). Claude's "*" is an
//     invalid regex there, so tool events use ".*". Only the tool
//     events (PreToolUse/PostToolUse) carry a matcher at all; the
//     lifecycle events (SessionStart/PromptSubmit/Notification/Stop)
//     use auggie's matcher-less group shape.
//   - Event identity: auggie tells the hook which event fired via the
//     AUGMENT_HOOK_EVENT environment variable, so our "command" is
//     just the script path. We still pass the event name in "args" as
//     a self-describing fallback; ownership is matched on "command".
//
// Hook entries fleet-man manages have this canonical shape (tool
// events also carry "matcher": ".*"):
//
//   {
//     "hooks": [
//       {"type": "command", "command": "<AuggieScriptSuffix path>", "args": ["<Event>"]}
//     ]
//   }

// ===========================================
// Constants
// ===========================================

// AuggieScriptSuffix is the home-relative path of the auggie
// state-writing hook script, the auggie counterpart to
// FleetManScriptSuffix. Suffix-based recognition (rather than
// full-path equality) lets us identify "our" entries without knowing
// the absolute path, so an entry written under a different $HOME is
// still recognised and updated rather than duplicated.
const AuggieScriptSuffix = ".fleet/scripts/auggie-state-hook.sh"

// auggieHookEvent pairs an auggie lifecycle event with whether its
// native settings schema accepts a tool-name matcher. Tool events
// (PreToolUse/PostToolUse) do; the lifecycle events do not.
type auggieHookEvent struct {
	name       string
	hasMatcher bool
}

// auggieManagedEvents lists the auggie lifecycle events fleet-man
// installs handlers for. Order governs first-insert order; subsequent
// reconciliations preserve whatever position our entry already holds.
var auggieManagedEvents = []auggieHookEvent{
	{name: "SessionStart", hasMatcher: false},
	{name: "PromptSubmit", hasMatcher: false},
	{name: "PreToolUse", hasMatcher: true},
	{name: "PostToolUse", hasMatcher: true},
	{name: "Notification", hasMatcher: false},
	{name: "Stop", hasMatcher: false},
}

// ===========================================
// Public API
// ===========================================

// InjectAuggieHooks upserts fleet-man's state-writing hook entries
// into an auggie settings.json document, returning the new bytes.
//
// current is the current contents of settings.json. nil, an empty
// slice, or whitespace-only input is treated as "the file does not
// exist yet" and yields a fresh document containing only our hooks.
//
// scriptPath is the absolute path of the in-container hook script,
// computed by the provisioner from the container's $HOME plus
// AuggieScriptSuffix.
//
// The guarantees match InjectFleetManHooks: foreign keys/entries are
// preserved exactly, the transformation is idempotent and
// position-stable, and nothing is written to disk (the caller owns the
// atomic write). An error is returned only when current is non-empty
// and not valid JSON, or when the parsed structure conflicts with the
// schema in a way we cannot safely repair.
func InjectAuggieHooks(current []byte, scriptPath string) ([]byte, error) {
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

	for _, event := range auggieManagedEvents {
		if err := auggieUpsertEvent(hooks, event, scriptPath); err != nil {
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

// auggieUpsertEvent reconciles fleet-man's group inside the array at
// hooks[event.name], replacing our existing group in place (collapsing
// duplicates) or appending it after all foreign entries. Foreign
// groups pass through unchanged. Returns an error if hooks[event] is
// present with a non-array type.
func auggieUpsertEvent(hooks map[string]any, event auggieHookEvent, scriptPath string) error {
	raw, present := hooks[event.name]
	var groups []any
	if present && raw != nil {
		existing, ok := raw.([]any)
		if !ok {
			return fmt.Errorf(`hooks[%q] must be a JSON array, got %T`, event.name, raw)
		}
		groups = existing
	}

	desired := auggieDesiredGroup(event, scriptPath)

	result := make([]any, 0, len(groups)+1)
	foundOurs := false
	for _, group := range groups {
		if isAuggieGroup(group) {
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
	hooks[event.name] = result
	return nil
}

// auggieDesiredGroup returns the canonical fleet-man hook group for the
// given event. Tool events carry the match-all ".*" regex matcher;
// lifecycle events use auggie's matcher-less group shape. A fresh map
// is built every call so later mutations cannot bleed into shared
// state.
func auggieDesiredGroup(event auggieHookEvent, scriptPath string) map[string]any {
	group := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": scriptPath,
				"args":    []any{event.name},
			},
		},
	}
	if event.hasMatcher {
		group["matcher"] = ".*"
	}
	return group
}

// isAuggieGroup reports whether the given hook group belongs to
// fleet-man, identified by the command path of any of its inner hooks.
// Robust to malformed entries: anything not matching the expected
// shape returns false (treat as foreign, leave alone).
func isAuggieGroup(group any) bool {
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
		if isAuggieCommand(command) {
			return true
		}
	}
	return false
}

// isAuggieCommand reports whether a hook command string was installed
// by fleet-man, matched by the home-relative AuggieScriptSuffix so
// entries written under a different $HOME are still recognised. The
// leading token is taken (defensive against a command that carries
// trailing arguments inline) before suffix-matching.
func isAuggieCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	first := command
	if idx := strings.IndexAny(command, " \t"); idx >= 0 {
		first = command[:idx]
	}
	return strings.HasSuffix(first, "/"+AuggieScriptSuffix)
}
