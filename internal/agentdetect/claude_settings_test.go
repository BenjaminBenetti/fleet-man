package agentdetect

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Test helpers
// ===========================================

// testScriptPath is the absolute hook-script path tests pretend the
// provisioner resolved from $HOME. The trailing component matches
// FleetManScriptSuffix so suffix-based recognition treats entries
// referencing it as fleet-man's.
const testScriptPath = "/home/test/" + FleetManScriptSuffix

// mustInject runs InjectFleetManHooks against testScriptPath and
// fails the test on any error. Returns the produced bytes.
func mustInject(t *testing.T, input []byte) []byte {
	t.Helper()
	out, err := InjectFleetManHooks(input, testScriptPath)
	if err != nil {
		t.Fatalf("InjectFleetManHooks(%q) returned error: %v", input, err)
	}
	return out
}

// parseRoot decodes settings JSON bytes into a map[string]any.
func parseRoot(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("output is not valid JSON: %v\nbytes: %s", err, raw)
	}
	return root
}

// hooksMap pulls root["hooks"] as a map, failing the test if absent
// or wrong-typed.
func hooksMap(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	raw, ok := root["hooks"]
	if !ok {
		t.Fatalf(`expected "hooks" key in root, got: %v`, root)
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf(`expected "hooks" to be an object, got %T`, raw)
	}
	return hooks
}

// eventGroups returns hooks[event] as []any, failing the test if
// missing or wrong-typed.
func eventGroups(t *testing.T, hooks map[string]any, event string) []any {
	t.Helper()
	raw, ok := hooks[event]
	if !ok {
		t.Fatalf("expected event %q in hooks, got: %v", event, hooks)
	}
	groups, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected hooks[%q] to be an array, got %T", event, raw)
	}
	return groups
}

// canonicalGroup returns the matcher-group entry fleet-man would
// produce on a fresh insertion for a given event when the
// provisioner resolved testScriptPath.
func canonicalGroup(event string) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": testScriptPath + " " + event,
			},
		},
	}
}

// findOurGroupIndex returns the position of fleet-man's group inside
// a matcher-group list, or -1 if absent.
func findOurGroupIndex(groups []any) int {
	for i, group := range groups {
		if isFleetManGroup(group) {
			return i
		}
	}
	return -1
}

// ===========================================
// Zero-state tests
// ===========================================

// TestInjectFleetManHooks_ZeroStates covers every "the file is in some
// pre-installation state" shape: missing entirely, empty, whitespace,
// empty object, hooks-null, hooks-empty, event-null, event-empty.
func TestInjectFleetManHooks_ZeroStates(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"nil input", nil},
		{"empty bytes", []byte{}},
		{"whitespace only", []byte("   \n\t  ")},
		{"empty object", []byte(`{}`)},
		{"hooks key null", []byte(`{"hooks": null}`)},
		{"hooks key empty object", []byte(`{"hooks": {}}`)},
		{"event key null", []byte(`{"hooks": {"PreToolUse": null}}`)},
		{"event key empty array", []byte(`{"hooks": {"PreToolUse": []}}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := mustInject(t, tc.input)
			root := parseRoot(t, out)
			hooks := hooksMap(t, root)

			for _, event := range fleetManManagedEvents {
				groups := eventGroups(t, hooks, event)
				if len(groups) != 1 {
					t.Fatalf("event %q: got %d groups, want 1", event, len(groups))
				}
				if !reflect.DeepEqual(groups[0], canonicalGroup(event)) {
					t.Errorf("event %q group mismatch:\n got: %#v\nwant: %#v",
						event, groups[0], canonicalGroup(event))
				}
			}
		})
	}
}

// ===========================================
// Foreign-content preservation tests
// ===========================================

// TestInjectFleetManHooks_PreservesUnrelatedTopLevelKeys verifies that
// keys at the document root that fleet-man knows nothing about
// survive a reconciliation pass byte-for-value.
func TestInjectFleetManHooks_PreservesUnrelatedTopLevelKeys(t *testing.T) {
	input := []byte(`{
		"theme": "dark",
		"telemetry": false,
		"customField": {"nested": [1, 2, 3]},
		"plugins": ["one", "two"]
	}`)

	out := mustInject(t, input)
	root := parseRoot(t, out)

	if got := root["theme"]; got != "dark" {
		t.Errorf("theme: got %v, want dark", got)
	}
	if got := root["telemetry"]; got != false {
		t.Errorf("telemetry: got %v, want false", got)
	}
	if got, ok := root["customField"].(map[string]any); !ok {
		t.Errorf("customField: not preserved as object, got %T", root["customField"])
	} else if nested, ok := got["nested"].([]any); !ok || len(nested) != 3 {
		t.Errorf("customField.nested: got %v", got["nested"])
	}
	if got, ok := root["plugins"].([]any); !ok || len(got) != 2 {
		t.Errorf("plugins: got %v", root["plugins"])
	}
	hooksMap(t, root) // hooks key was added
}

// TestInjectFleetManHooks_PreservesUnrelatedEvents verifies that hook
// events fleet-man does not manage are passed through verbatim.
func TestInjectFleetManHooks_PreservesUnrelatedEvents(t *testing.T) {
	input := []byte(`{
		"hooks": {
			"SessionStart": [
				{"matcher": "startup", "hooks": [{"type": "command", "command": "/bin/user-script"}]}
			],
			"FileChanged": [
				{"matcher": "*", "hooks": [{"type": "command", "command": "/usr/local/bin/watcher"}]}
			]
		}
	}`)

	out := mustInject(t, input)
	hooks := hooksMap(t, parseRoot(t, out))

	sessionStart := eventGroups(t, hooks, "SessionStart")
	if len(sessionStart) != 1 {
		t.Fatalf("SessionStart: got %d groups, want 1", len(sessionStart))
	}
	sessionGroup := sessionStart[0].(map[string]any)
	if sessionGroup["matcher"] != "startup" {
		t.Errorf("SessionStart matcher: got %v, want startup", sessionGroup["matcher"])
	}

	fileChanged := eventGroups(t, hooks, "FileChanged")
	if len(fileChanged) != 1 {
		t.Fatalf("FileChanged: got %d groups, want 1", len(fileChanged))
	}
}

// TestInjectFleetManHooks_AppendsAlongsideForeignEntries verifies
// that when the user already has hook entries for an event we
// manage, our entry is appended without disturbing theirs.
func TestInjectFleetManHooks_AppendsAlongsideForeignEntries(t *testing.T) {
	input := []byte(`{
		"hooks": {
			"PreToolUse": [
				{"matcher": "Bash", "hooks": [{"type": "command", "command": "/home/user/audit.sh"}]},
				{"matcher": "Edit", "hooks": [{"type": "command", "command": "/home/user/lint.sh"}]}
			]
		}
	}`)

	out := mustInject(t, input)
	groups := eventGroups(t, hooksMap(t, parseRoot(t, out)), "PreToolUse")

	if len(groups) != 3 {
		t.Fatalf("PreToolUse: got %d groups, want 3 (2 user + 1 ours)", len(groups))
	}
	// User's entries unchanged at positions 0 and 1.
	if group := groups[0].(map[string]any); group["matcher"] != "Bash" {
		t.Errorf("position 0: matcher got %v, want Bash", group["matcher"])
	}
	if group := groups[1].(map[string]any); group["matcher"] != "Edit" {
		t.Errorf("position 1: matcher got %v, want Edit", group["matcher"])
	}
	// Ours appended at the end.
	if !reflect.DeepEqual(groups[2], canonicalGroup("PreToolUse")) {
		t.Errorf("position 2: got %#v, want canonical fleet-man entry", groups[2])
	}
}

// ===========================================
// Idempotency tests
// ===========================================

// TestInjectFleetManHooks_Idempotent verifies that running the
// reconciliation twice produces identical bytes the second time —
// the canonical correctness guarantee for any reconcile loop.
func TestInjectFleetManHooks_Idempotent(t *testing.T) {
	first := mustInject(t, nil)
	second := mustInject(t, first)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("output not idempotent\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestInjectFleetManHooks_IdempotentOnComplexInput exercises
// idempotency through a document containing foreign entries
// alongside ours.
func TestInjectFleetManHooks_IdempotentOnComplexInput(t *testing.T) {
	input := []byte(`{
		"theme": "dark",
		"hooks": {
			"PreToolUse": [
				{"matcher": "Bash", "hooks": [{"type": "command", "command": "/u/audit.sh"}]}
			],
			"SessionStart": [
				{"matcher": "*", "hooks": [{"type": "command", "command": "/u/init.sh"}]}
			]
		}
	}`)

	first := mustInject(t, input)
	second := mustInject(t, first)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("not idempotent on complex input\nfirst:  %s\nsecond: %s", first, second)
	}
}

// ===========================================
// Stale-entry replacement tests
// ===========================================

// TestInjectFleetManHooks_ReplacesStaleEntry verifies that an
// existing fleet-man entry with outdated content (e.g. an old event
// argument) is replaced rather than duplicated.
func TestInjectFleetManHooks_ReplacesStaleEntry(t *testing.T) {
	input := fmt.Appendf(nil, `{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "old-matcher",
					"hooks": [{"type": "command", "command": "%s OldEventName"}]
				}
			]
		}
	}`, testScriptPath)

	out := mustInject(t, input)
	groups := eventGroups(t, hooksMap(t, parseRoot(t, out)), "PreToolUse")

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1 (replacement, no duplicate)", len(groups))
	}
	if !reflect.DeepEqual(groups[0], canonicalGroup("PreToolUse")) {
		t.Errorf("entry not refreshed: got %#v", groups[0])
	}
}

// TestInjectFleetManHooks_PreservesPositionDuringReplace verifies that
// when our stale entry is sandwiched between foreign entries, the
// fresh entry takes the same position rather than moving to the end.
func TestInjectFleetManHooks_PreservesPositionDuringReplace(t *testing.T) {
	input := fmt.Appendf(nil, `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "Bash", "hooks": [{"type": "command", "command": "/u/before.sh"}]},
				{"matcher": "old", "hooks": [{"type": "command", "command": "%s stale"}]},
				{"matcher": "Edit", "hooks": [{"type": "command", "command": "/u/after.sh"}]}
			]
		}
	}`, testScriptPath)

	out := mustInject(t, input)
	groups := eventGroups(t, hooksMap(t, parseRoot(t, out)), "PreToolUse")

	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	if findOurGroupIndex(groups) != 1 {
		t.Errorf("our group moved: index = %d, want 1", findOurGroupIndex(groups))
	}
	if group := groups[0].(map[string]any); group["matcher"] != "Bash" {
		t.Errorf("position 0 corrupted: %v", group)
	}
	if group := groups[2].(map[string]any); group["matcher"] != "Edit" {
		t.Errorf("position 2 corrupted: %v", group)
	}
}

// TestInjectFleetManHooks_DedupesMultipleStaleEntries verifies that
// if the file somehow ended up with several fleet-man entries for
// the same event, reconciliation collapses them to one at the
// position of the first occurrence.
func TestInjectFleetManHooks_DedupesMultipleStaleEntries(t *testing.T) {
	input := fmt.Appendf(nil, `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "*", "hooks": [{"type": "command", "command": "%[1]s v1"}]},
				{"matcher": "Bash", "hooks": [{"type": "command", "command": "/u/audit.sh"}]},
				{"matcher": "*", "hooks": [{"type": "command", "command": "%[1]s v2"}]},
				{"matcher": "*", "hooks": [{"type": "command", "command": "%[1]s v3"}]}
			]
		}
	}`, testScriptPath)

	out := mustInject(t, input)
	groups := eventGroups(t, hooksMap(t, parseRoot(t, out)), "PreToolUse")

	// Three duplicates collapse to one; foreign entry preserved.
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (dedupe to one + foreign)", len(groups))
	}
	if findOurGroupIndex(groups) != 0 {
		t.Errorf("dedup landed at wrong index: %d, want 0 (first occurrence)", findOurGroupIndex(groups))
	}
	if group := groups[1].(map[string]any); group["matcher"] != "Bash" {
		t.Errorf("foreign entry corrupted: %v", group)
	}
}

// ===========================================
// Recognition tests
// ===========================================

// TestIsFleetManCommand verifies the suffix-based command-path
// matcher does not false-positive on look-alike paths and does not
// false-negative on our own command with arguments. Also covers the
// cross-home-dir case: a path under a different $HOME but with the
// same FleetManScriptSuffix is recognised, which is what makes
// re-provisioning safe across remoteUser changes.
func TestIsFleetManCommand(t *testing.T) {
	otherHomePath := "/root/" + FleetManScriptSuffix
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"exact match no args", testScriptPath, true},
		{"with single arg", testScriptPath + " PreToolUse", true},
		{"with multiple args", testScriptPath + " PreToolUse extra", true},
		{"tab-separated arg", testScriptPath + "\tPreToolUse", true},
		{"leading whitespace", "  " + testScriptPath + " Stop", true},
		{"different $HOME, same suffix", otherHomePath + " Stop", true},
		{"prefix lookalike", testScriptPath + "-other", false},
		{"empty", "", false},
		{"unrelated path", "/home/user/some-script.sh", false},
		{"shell wrapper", "sh -c " + testScriptPath, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFleetManCommand(tc.cmd); got != tc.want {
				t.Errorf("isFleetManCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestInjectFleetManHooks_LookalikeNotConfusedAsOurs ensures a user
// hook whose path merely shares a prefix with ours is left alone
// rather than being treated as a stale fleet-man entry.
func TestInjectFleetManHooks_LookalikeNotConfusedAsOurs(t *testing.T) {
	input := fmt.Appendf(nil, `{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "*",
					"hooks": [{"type": "command", "command": "%s-impostor"}]
				}
			]
		}
	}`, testScriptPath)

	out := mustInject(t, input)
	groups := eventGroups(t, hooksMap(t, parseRoot(t, out)), "PreToolUse")

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (lookalike preserved + ours appended)", len(groups))
	}
	// Lookalike still at position 0.
	group := groups[0].(map[string]any)
	inner := group["hooks"].([]any)
	cmd := inner[0].(map[string]any)["command"].(string)
	if !strings.HasSuffix(cmd, "impostor") {
		t.Errorf("lookalike entry mutated: command=%q", cmd)
	}
}

// ===========================================
// Error tests
// ===========================================

// TestInjectFleetManHooks_RejectsMalformedInput verifies that the
// function refuses to operate on inputs whose structure could not be
// safely repaired in place. The contract is to return an error, so
// the caller leaves the file untouched on disk.
func TestInjectFleetManHooks_RejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"not json", []byte("this is not json")},
		{"truncated json", []byte(`{"hooks":`)},
		{"hooks is string", []byte(`{"hooks": "wat"}`)},
		{"hooks is array", []byte(`{"hooks": []}`)},
		{"event is string", []byte(`{"hooks": {"PreToolUse": "wat"}}`)},
		{"event is object", []byte(`{"hooks": {"PreToolUse": {"matcher": "*"}}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := InjectFleetManHooks(tc.input, testScriptPath); err == nil {
				t.Fatalf("InjectFleetManHooks(%q) = nil, want error", tc.input)
			}
		})
	}
}

// ===========================================
// Output format tests
// ===========================================

// TestInjectFleetManHooks_OutputIsValidIndentedJSON sanity-checks that
// the result is parseable, indented for readability, and ends with a
// trailing newline (POSIX file convention so editors don't whine).
func TestInjectFleetManHooks_OutputIsValidIndentedJSON(t *testing.T) {
	out := mustInject(t, nil)
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("output should end with newline, got: %q", out)
	}
	if !strings.Contains(string(out), "  ") {
		t.Error("output should be indented (2 spaces), got compact JSON")
	}
	parseRoot(t, out)
}

// TestInjectFleetManHooks_AllManagedEventsCovered guards against
// silent regressions if fleetManManagedEvents is edited — every
// listed event must end up in the output.
func TestInjectFleetManHooks_AllManagedEventsCovered(t *testing.T) {
	out := mustInject(t, nil)
	hooks := hooksMap(t, parseRoot(t, out))
	for _, event := range fleetManManagedEvents {
		if _, ok := hooks[event]; !ok {
			t.Errorf("managed event %q missing from output", event)
		}
	}
}

// TestCaptureAllScript_HookPathInLockstep guards against drift between
// FleetManScriptSuffix (the canonical hook-script home-relative path)
// and the literal embedded in backend.CaptureAllScript. The capture
// script cannot import this package, so the path is duplicated; this
// test fails fast if someone updates one without the other and would
// otherwise leave the host triggering reprovisions for a perfectly
// healthy container, or silently failing to detect a missing script.
func TestCaptureAllScript_HookPathInLockstep(t *testing.T) {
	want := "$HOME/" + FleetManScriptSuffix
	if !strings.Contains(backend.CaptureAllScript, want) {
		t.Errorf("backend.CaptureAllScript does not reference %q — it must check the same path the provisioner installs to", want)
	}
}
