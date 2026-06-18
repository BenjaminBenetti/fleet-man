package agentdetect

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// auggieTestScriptPath is a stable absolute path ending in the auggie
// suffix, used across the injection tests.
const auggieTestScriptPath = "/home/test/" + AuggieScriptSuffix

// mustInjectAuggie runs InjectAuggieHooks against auggieTestScriptPath
// and fails the test on error.
func mustInjectAuggie(t *testing.T, input []byte) []byte {
	t.Helper()
	out, err := InjectAuggieHooks(input, auggieTestScriptPath)
	if err != nil {
		t.Fatalf("InjectAuggieHooks(%q) returned error: %v", input, err)
	}
	return out
}

// parseHooks unmarshals settings.json bytes and returns the "hooks"
// object, failing the test if the document is not the expected shape.
func parseHooks(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, doc)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks is not an object: %T", root["hooks"])
	}
	return hooks
}

// fleetManAuggieGroup returns the single fleet-man-owned group for the
// given event from a parsed hooks map (or fails if absent/duplicated).
func fleetManAuggieGroup(t *testing.T, hooks map[string]any, event string) map[string]any {
	t.Helper()
	arr, ok := hooks[event].([]any)
	if !ok {
		t.Fatalf("hooks[%q] is not an array: %T", event, hooks[event])
	}
	var found map[string]any
	for _, g := range arr {
		if isAuggieGroup(g) {
			if found != nil {
				t.Fatalf("hooks[%q] has more than one fleet-man group", event)
			}
			found = g.(map[string]any)
		}
	}
	if found == nil {
		t.Fatalf("hooks[%q] has no fleet-man group", event)
	}
	return found
}

// TestInjectAuggieHooks_FreshShape verifies the canonical structure a
// fresh injection produces: every managed event present, tool events
// carrying the ".*" regex matcher, lifecycle events matcher-less, and
// each inner hook pointing at the script with the event in "args".
func TestInjectAuggieHooks_FreshShape(t *testing.T) {
	out := mustInjectAuggie(t, nil)

	// auggie's matcher is a regex; Claude's "*" would be invalid there.
	if bytes.Contains(out, []byte(`"matcher": "*"`)) {
		t.Errorf("output uses the invalid-regex matcher \"*\":\n%s", out)
	}

	hooks := parseHooks(t, out)

	withMatcher := map[string]bool{
		"SessionStart": false,
		"PromptSubmit": false,
		"PreToolUse":   true,
		"PostToolUse":  true,
		"Notification": false,
		"Stop":         false,
	}

	for event, wantMatcher := range withMatcher {
		group := fleetManAuggieGroup(t, hooks, event)

		matcher, hasMatcher := group["matcher"]
		if wantMatcher {
			if !hasMatcher {
				t.Errorf("%s: expected a matcher, got none", event)
			} else if matcher != ".*" {
				t.Errorf("%s: matcher = %v, want \".*\"", event, matcher)
			}
		} else if hasMatcher {
			t.Errorf("%s: lifecycle event should have no matcher, got %v", event, matcher)
		}

		inner, ok := group["hooks"].([]any)
		if !ok || len(inner) != 1 {
			t.Fatalf("%s: inner hooks malformed: %v", event, group["hooks"])
		}
		hook := inner[0].(map[string]any)
		if hook["type"] != "command" {
			t.Errorf("%s: type = %v, want command", event, hook["type"])
		}
		if hook["command"] != auggieTestScriptPath {
			t.Errorf("%s: command = %v, want %q", event, hook["command"], auggieTestScriptPath)
		}
		// The event identity rides in args, NOT appended to command.
		args, ok := hook["args"].([]any)
		if !ok || len(args) != 1 || args[0] != event {
			t.Errorf("%s: args = %v, want [%q]", event, hook["args"], event)
		}
	}
}

// TestInjectAuggieHooks_Idempotent verifies running the function on its
// own output is a no-op.
func TestInjectAuggieHooks_Idempotent(t *testing.T) {
	first := mustInjectAuggie(t, nil)
	second := mustInjectAuggie(t, first)
	if !bytes.Equal(first, second) {
		t.Errorf("not idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestInjectAuggieHooks_PreservesForeignContent verifies user keys and
// foreign hook entries round-trip untouched.
func TestInjectAuggieHooks_PreservesForeignContent(t *testing.T) {
	existing := []byte(`{
  "model": "augment-default",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/u/audit.sh"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/u/onstop.sh"}]}
    ]
  }
}`)
	out := mustInjectAuggie(t, existing)

	if !bytes.Contains(out, []byte(`"model"`)) || !bytes.Contains(out, []byte("augment-default")) {
		t.Errorf("user model key dropped:\n%s", out)
	}
	if !bytes.Contains(out, []byte("/u/audit.sh")) {
		t.Errorf("foreign PreToolUse hook dropped:\n%s", out)
	}
	if !bytes.Contains(out, []byte("/u/onstop.sh")) {
		t.Errorf("foreign Stop hook dropped:\n%s", out)
	}

	// Our entry should sit alongside the foreign ones, not replace them.
	hooks := parseHooks(t, out)
	preArr := hooks["PreToolUse"].([]any)
	if len(preArr) != 2 {
		t.Errorf("PreToolUse should have foreign + ours = 2 groups, got %d", len(preArr))
	}
}

// TestInjectAuggieHooks_ReplacesStaleEntryAcrossHome verifies that a
// prior fleet-man entry written under a DIFFERENT $HOME is recognised
// by suffix and updated in place rather than duplicated.
func TestInjectAuggieHooks_ReplacesStaleEntryAcrossHome(t *testing.T) {
	stalePath := "/home/other/" + AuggieScriptSuffix
	existing := []byte(`{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "` + stalePath + `", "args": ["Stop"]}]}
    ]
  }
}`)
	out := mustInjectAuggie(t, existing)

	hooks := parseHooks(t, out)
	arr := hooks["Stop"].([]any)
	if len(arr) != 1 {
		t.Fatalf("Stop should have exactly one (updated) fleet-man group, got %d:\n%s", len(arr), out)
	}
	group := fleetManAuggieGroup(t, hooks, "Stop")
	hook := group["hooks"].([]any)[0].(map[string]any)
	if hook["command"] != auggieTestScriptPath {
		t.Errorf("stale command not updated: %v", hook["command"])
	}
	if bytes.Contains(out, []byte(stalePath)) {
		t.Errorf("stale path still present after update:\n%s", out)
	}
}

// TestInjectAuggieHooks_RejectsMalformedInput verifies invalid JSON is
// surfaced as an error rather than silently overwriting the file.
func TestInjectAuggieHooks_RejectsMalformedInput(t *testing.T) {
	if _, err := InjectAuggieHooks([]byte(`{not json`), auggieTestScriptPath); err == nil {
		t.Fatal("expected error on malformed input, got nil")
	}
}

// TestCaptureAllScript_AuggieHookPathInLockstep guards against drift
// between AuggieScriptSuffix and the literal embedded in
// backend.CaptureAllScript. The capture script checks the auggie hook
// is installed and re-provisions when it is gone; if the paths drift,
// the self-heal would either never fire or fire forever.
func TestCaptureAllScript_AuggieHookPathInLockstep(t *testing.T) {
	want := "/" + AuggieScriptSuffix
	if !strings.Contains(backend.CaptureAllScript, want) {
		t.Errorf("backend.CaptureAllScript does not reference %q — it must check the same path the provisioner installs to", want)
	}
}
