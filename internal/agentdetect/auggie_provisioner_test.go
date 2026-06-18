package agentdetect

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestAuggieProvision_FreshContainer covers the common case: a new
// container with no ~/.augment/settings.json. The provisioner queries
// $HOME, drops the auggie hook script, reads (empty), and writes a
// freshly-generated settings.json under ~/.augment.
func TestAuggieProvision_FreshContainer(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil}, // query home
			{stdout: nil, err: nil},                // drop script
			{stdout: nil, err: nil},                // read settings (empty)
			{stdout: nil, err: nil},                // write settings
		},
	}
	if err := NewAuggieProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(exec.calls) != 4 {
		t.Fatalf("expected 4 exec calls, got %d", len(exec.calls))
	}
	wantKinds := []string{"query-home", "drop-script", "read-settings", "write-settings"}
	for i, want := range wantKinds {
		if got := callKind(exec.calls[i]); got != want {
			t.Errorf("call %d kind = %q, want %q", i, got, want)
		}
	}

	// The hook script payload must be the embedded auggie bytes.
	if !bytes.Equal(exec.calls[1].stdin, AuggieHookScript) {
		t.Errorf("drop-script stdin does not match embedded AuggieHookScript")
	}

	// The drop-script command references the resolved absolute path.
	expectedScriptPath := "/home/test/" + AuggieScriptSuffix
	if dropBody := exec.calls[1].args[2]; !strings.Contains(dropBody, expectedScriptPath) {
		t.Errorf("drop-script missing resolved path %q:\n%s", expectedScriptPath, dropBody)
	}

	// Read and write must target ~/.augment/settings.json, not ~/.claude.
	if body := exec.calls[2].args[2]; !strings.Contains(body, ".augment/settings.json") {
		t.Errorf("read does not target ~/.augment/settings.json:\n%s", body)
	}
	if body := exec.calls[3].args[2]; !strings.Contains(body, ".augment/settings.json") {
		t.Errorf("write does not target ~/.augment/settings.json:\n%s", body)
	}

	// The written settings.json must register every managed event,
	// each pointing at the resolved script path.
	written := exec.calls[3].stdin
	hooks := parseHooks(t, written)
	for _, event := range auggieManagedEvents {
		group := fleetManAuggieGroup(t, hooks, event.name)
		hook := group["hooks"].([]any)[0].(map[string]any)
		if hook["command"] != expectedScriptPath {
			t.Errorf("%s command = %v, want %q", event.name, hook["command"], expectedScriptPath)
		}
	}
}

// TestAuggieProvision_PreservesExistingSettings verifies user content
// round-trips through the provisioner unchanged.
func TestAuggieProvision_PreservesExistingSettings(t *testing.T) {
	existing := []byte(`{"model":"x","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/u/audit.sh"}]}]}}`)
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: existing, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewAuggieProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	written := string(exec.calls[3].stdin)
	if !strings.Contains(written, `"model"`) || !strings.Contains(written, "/u/audit.sh") {
		t.Errorf("user content dropped from output:\n%s", written)
	}
}

// TestAuggieProvision_Idempotent runs the provisioner twice (second
// read replays the first write) and asserts byte-for-byte stability.
func TestAuggieProvision_Idempotent(t *testing.T) {
	first := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewAuggieProvisioner(first).Provision(); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	firstWritten := first.calls[3].stdin

	second := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: firstWritten, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewAuggieProvisioner(second).Provision(); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if !bytes.Equal(firstWritten, second.calls[3].stdin) {
		t.Errorf("not idempotent\nfirst:  %s\nsecond: %s", firstWritten, second.calls[3].stdin)
	}
}

// TestAuggieProvision_DropFailureSkipsSettings verifies the provisioner
// short-circuits when the script-drop fails: settings are untouched.
func TestAuggieProvision_DropFailureSkipsSettings(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: errors.New("chmod denied")},
		},
	}
	err := NewAuggieProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected error from drop failure, got nil")
	}
	if !strings.Contains(err.Error(), "install hook script") {
		t.Errorf("error not phase-tagged: %v", err)
	}
	if len(exec.calls) != 2 {
		t.Errorf("expected 2 calls (home, drop), got %d", len(exec.calls))
	}
}
