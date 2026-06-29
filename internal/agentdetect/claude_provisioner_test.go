package agentdetect

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

// ===========================================
// Stub executor
// ===========================================

// fakeCall captures one Run invocation against the stub executor.
type fakeCall struct {
	args  []string
	stdin []byte
}

// fakeResponse pairs canned stdout with an optional error so the
// stub can replay backend behaviour deterministically.
type fakeResponse struct {
	stdout []byte
	err    error
}

// copyCall captures one CopyFile invocation (the settings write, now streamed
// through the backend's uncapped CopyFile rather than an inline Run).
type copyCall struct {
	path    string
	content []byte
	mode    int
}

// fakeExec is a ContainerExecutor that records calls and returns
// pre-seeded responses in order. Tests use it to assert both that
// the provisioner issued the expected sequence of shell
// invocations AND that error paths short-circuit the right way.
type fakeExec struct {
	calls     []fakeCall
	responses []fakeResponse
	copies    []copyCall // recorded CopyFile invocations (settings writes)
	copyErr   error      // error CopyFile returns, for the write-failure paths
}

// Run satisfies ContainerExecutor.
func (f *fakeExec) Run(args []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	if len(f.calls) > len(f.responses) {
		return nil, errors.New("fakeExec: unexpected extra call")
	}
	resp := f.responses[len(f.calls)-1]
	return resp.stdout, resp.err
}

// CopyFile satisfies ContainerExecutor, recording the streamed payload so tests
// can assert on the written settings.json without an inline base64 decode.
func (f *fakeExec) CopyFile(src io.Reader, remotePath string, mode int) error {
	data, _ := io.ReadAll(src)
	f.copies = append(f.copies, copyCall{path: remotePath, content: data, mode: mode})
	return f.copyErr
}

// callKind classifies a recorded Run call by inspecting its shell
// payload — this lets tests assert "the third call read
// settings.json" without depending on the exact wording of the
// shell snippet. The hook script drop is an inline (base64-in-argv)
// write, recognised by its in-container `base64 -d` decode; the
// settings WRITE no longer appears here at all (it streams through
// CopyFile, asserted via fakeExec.copies). The write-settings case
// is retained only to label any stray inline settings write.
func callKind(call fakeCall) string {
	if len(call.args) < 3 {
		return "unknown"
	}
	body := call.args[2]
	switch {
	case strings.Contains(body, "echo") && strings.Contains(body, "$HOME"):
		return "query-home"
	case strings.Contains(body, "if [ -f") && strings.Contains(body, "settings.json"):
		return "read-settings"
	case strings.Contains(body, "base64 -d") && strings.Contains(body, "settings.json"):
		return "write-settings"
	case strings.Contains(body, "base64 -d"):
		return "drop-script"
	}
	return "unknown"
}

// inlineWritten extracts and decodes the payload an inline (base64-in-argv)
// write embeds in its shell body (between `printf %s '` and `'`), so tests can
// assert on the bytes the provisioner wrote without a stdin pipe.
func inlineWritten(call fakeCall) []byte {
	if len(call.args) < 3 {
		return nil
	}
	const pre = "printf %s '"
	body := call.args[2]
	i := strings.Index(body, pre)
	if i < 0 {
		return nil
	}
	rest := body[i+len(pre):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		return nil
	}
	return data
}

// homeStdout is the canned response to the queryHome call — used by
// every happy-path test so the resolved script path is predictable.
const homeStdout = "/home/test\n"

// ===========================================
// Provisioning happy paths
// ===========================================

// TestProvision_FreshContainer covers the most common case: a brand
// new container with no settings.json. The provisioner should query
// $HOME, drop the script, read (empty), and write a freshly-
// generated settings.json.
func TestProvision_FreshContainer(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil}, // query home
			{stdout: nil, err: nil},                // drop script
			{stdout: nil, err: nil},                // read settings (empty file)
		},
	}
	if err := NewClaudeProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// The settings write now streams through CopyFile, so only three Run calls
	// remain (home, drop, read) plus one recorded copy.
	if len(exec.calls) != 3 {
		t.Fatalf("expected 3 exec calls, got %d", len(exec.calls))
	}
	wantKinds := []string{"query-home", "drop-script", "read-settings"}
	for i, want := range wantKinds {
		if got := callKind(exec.calls[i]); got != want {
			t.Errorf("call %d kind = %q, want %q", i, got, want)
		}
	}
	if len(exec.copies) != 1 {
		t.Fatalf("expected 1 settings CopyFile, got %d", len(exec.copies))
	}
	if want := "/home/test/" + settingsSuffix; exec.copies[0].path != want {
		t.Errorf("settings written to %q, want %q", exec.copies[0].path, want)
	}

	// The hook script payload sent over stdin must match the
	// embedded bytes — proves the wrong file isn't being shipped.
	if !bytes.Equal(inlineWritten(exec.calls[1]), ClaudeHookScript) {
		t.Errorf("drop-script stdin does not match embedded ClaudeHookScript")
	}

	// The drop-script command must reference the resolved absolute
	// path, which is $HOME (from query) joined with the suffix.
	expectedScriptPath := "/home/test/" + FleetManScriptSuffix
	dropBody := exec.calls[1].args[2]
	if !strings.Contains(dropBody, expectedScriptPath) {
		t.Errorf("drop-script missing resolved path %q:\n%s", expectedScriptPath, dropBody)
	}

	// The written settings.json must contain our hook command path
	// for every managed event, using the resolved absolute path.
	written := string(exec.copies[0].content)
	for _, event := range fleetManManagedEvents {
		marker := expectedScriptPath + " " + event
		if !strings.Contains(written, marker) {
			t.Errorf("written settings.json missing %q\nbody:\n%s", marker, written)
		}
	}
}

// TestProvision_PreservesExistingSettings verifies that user
// content in settings.json round-trips through the provisioner
// unchanged — critical safety property for editing a user file.
func TestProvision_PreservesExistingSettings(t *testing.T) {
	existing := []byte(`{"theme":"dark","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/u/audit.sh"}]}]}}`)

	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: existing, err: nil},
		},
	}
	if err := NewClaudeProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	written := string(exec.copies[0].content)
	if !strings.Contains(written, `"theme"`) || !strings.Contains(written, `"dark"`) {
		t.Errorf("user theme key dropped from output:\n%s", written)
	}
	if !strings.Contains(written, "/u/audit.sh") {
		t.Errorf("user PreToolUse audit hook dropped from output:\n%s", written)
	}
}

// TestProvision_Idempotent runs the provisioner twice (with the
// second read returning the first write's output) and asserts the
// settings written on the second pass match the first byte-for-
// byte. End-to-end idempotency, not just InjectFleetManHooks's.
func TestProvision_Idempotent(t *testing.T) {
	first := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewClaudeProvisioner(first).Provision(); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	firstWritten := first.copies[0].content

	second := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: firstWritten, err: nil},
		},
	}
	if err := NewClaudeProvisioner(second).Provision(); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	secondWritten := second.copies[0].content

	if !bytes.Equal(firstWritten, secondWritten) {
		t.Errorf("not idempotent\nfirst:  %s\nsecond: %s", firstWritten, secondWritten)
	}
}

// TestProvision_ScriptUsesCanonicalSuffix asserts the drop-script
// command embeds the canonical script suffix derived from $HOME,
// guarding against drift between the script path used to write the
// file and the path written into settings.json.
func TestProvision_ScriptUsesCanonicalSuffix(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewClaudeProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	dropBody := exec.calls[1].args[2]
	if !strings.Contains(dropBody, FleetManScriptSuffix) {
		t.Errorf("drop-script body missing FleetManScriptSuffix %q:\n%s",
			FleetManScriptSuffix, dropBody)
	}
}

// TestProvision_HomeWithTrailingWhitespace verifies the queryHome
// step trims shell-typical trailing newlines/whitespace before
// using the value. A bare `echo "$HOME"` always produces a trailing
// newline and we must not bake that into a path.
func TestProvision_HomeWithTrailingWhitespace(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte("/home/vscode  \n"), err: nil}, // padded
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
		},
	}
	if err := NewClaudeProvisioner(exec).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	dropBody := exec.calls[1].args[2]
	expected := "/home/vscode/" + FleetManScriptSuffix
	if !strings.Contains(dropBody, expected) {
		t.Errorf("expected trimmed path %q in drop body:\n%s", expected, dropBody)
	}
	// And no whitespace bleed through.
	if strings.Contains(dropBody, "/home/vscode  /") || strings.Contains(dropBody, "/home/vscode\n") {
		t.Errorf("queryHome whitespace leaked into resolved path:\n%s", dropBody)
	}
}

// ===========================================
// Provisioning failure modes
// ===========================================

// TestProvision_HomeQueryFailureAborts verifies the provisioner
// aborts cleanly if it cannot determine $HOME — without that we
// would not know where to drop the script, so attempting subsequent
// steps would corrupt either /tmp or some incorrect path.
func TestProvision_HomeQueryFailureAborts(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: nil, err: errors.New("exec lost")},
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected error when home query fails, got nil")
	}
	if !strings.Contains(err.Error(), "resolve home") {
		t.Errorf("error not phase-tagged: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Errorf("expected 1 call (home query only), got %d", len(exec.calls))
	}
}

// TestProvision_EmptyHomeAborts verifies that a successful but
// empty home query is also treated as an abort condition — without
// a valid path we cannot proceed.
func TestProvision_EmptyHomeAborts(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte("\n"), err: nil}, // empty after trim
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected error when home is empty")
	}
}

// TestProvision_DropFailureSkipsSettings verifies the provisioner
// short-circuits when the script-drop fails: settings are not
// touched. Important so a permission failure does not leave the
// file in an inconsistent in-between state.
func TestProvision_DropFailureSkipsSettings(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: errors.New("chmod denied")},
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
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

// TestProvision_ReadFailureSurfaces verifies a read error (e.g.,
// permission issue on settings.json) is surfaced rather than
// silently swallowed.
func TestProvision_ReadFailureSurfaces(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: errors.New("perm denied")},
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected error from read failure")
	}
	if !strings.Contains(err.Error(), "edit settings.json") {
		t.Errorf("error not phase-tagged: %v", err)
	}
	if len(exec.calls) != 3 {
		t.Errorf("expected 3 calls (home, drop, read), got %d", len(exec.calls))
	}
}

// TestProvision_MalformedExistingSettingsSurfaces verifies a
// settings.json that fails to parse causes Provision to abort
// before writing anything — never overwriting the user's possibly-
// hand-edited (or corrupted) file with our generated one.
func TestProvision_MalformedExistingSettingsSurfaces(t *testing.T) {
	exec := &fakeExec{
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: []byte(`{not valid json`), err: nil},
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected parse error to bubble up")
	}
	// No write call should have been issued.
	if len(exec.calls) != 3 {
		t.Errorf("write should not have been attempted, got %d calls", len(exec.calls))
	}
}

// TestProvision_WriteFailureSurfaces verifies that even if read &
// compute succeed, a write error bubbles up with phase context.
func TestProvision_WriteFailureSurfaces(t *testing.T) {
	exec := &fakeExec{
		copyErr: errors.New("disk full"), // the settings write now fails via CopyFile
		responses: []fakeResponse{
			{stdout: []byte(homeStdout), err: nil},
			{stdout: nil, err: nil},
			{stdout: nil, err: nil},
		},
	}
	err := NewClaudeProvisioner(exec).Provision()
	if err == nil {
		t.Fatal("expected write error")
	}
	if !strings.Contains(err.Error(), "edit settings.json") {
		t.Errorf("error not phase-tagged: %v", err)
	}
}
