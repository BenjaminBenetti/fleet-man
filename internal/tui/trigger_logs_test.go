package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestTriggerLogsKeyEmpty: 'L' on a trigger with no recorded firings reports
// "no events" and launches no pager.
func TestTriggerLogsKeyEmpty(t *testing.T) {
	orig := triggerLogsRemote
	t.Cleanup(func() { triggerLogsRemote = orig })
	triggerLogsRemote = func(string, string) (string, error) { return "", nil }

	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowTrigger)
	if cmd := fp.Update(m, key('L')); cmd != nil {
		t.Fatal("an empty history must not launch a pager")
	}
	if !strings.Contains(m.message, "No events") {
		t.Errorf("message = %q, want a no-events notice", m.message)
	}
}

// TestTriggerLogsKeyError: a fetch failure surfaces as a status message, no pager.
func TestTriggerLogsKeyError(t *testing.T) {
	orig := triggerLogsRemote
	t.Cleanup(func() { triggerLogsRemote = orig })
	triggerLogsRemote = func(string, string) (string, error) { return "", errors.New("boom") }

	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowTrigger)
	if cmd := fp.Update(m, key('L')); cmd != nil {
		t.Fatal("a fetch error must not launch a pager")
	}
	if !strings.Contains(m.message, "boom") {
		t.Errorf("message = %q, want the fetch error", m.message)
	}
}

// TestTriggerLogsKeyLaunches: 'L' on a trigger with recorded firings fetches the
// right trigger's logs and returns a pager command. TMPDIR is redirected so the
// temp log file lands in the test's auto-cleaned dir.
func TestTriggerLogsKeyLaunches(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	orig := triggerLogsRemote
	t.Cleanup(func() { triggerLogsRemote = orig })
	var gotFleet, gotTrigger string
	triggerLogsRemote = func(f, tr string) (string, error) {
		gotFleet, gotTrigger = f, tr
		return "the recorded payload\n", nil
	}

	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowTrigger)
	cmd := fp.Update(m, key('L'))
	if cmd == nil {
		t.Fatal("recorded firings should launch a pager")
	}
	if gotFleet != "alpha" || gotTrigger != "nightly" {
		t.Errorf("fetched logs for %q/%q, want alpha/nightly", gotFleet, gotTrigger)
	}
}

// TestTriggerLogsKeyIgnoredOffTrigger: 'L' on a non-trigger row never calls the
// trigger-logs fetch (it falls through to the instance-logs path).
func TestTriggerLogsKeyIgnoredOffTrigger(t *testing.T) {
	orig := triggerLogsRemote
	t.Cleanup(func() { triggerLogsRemote = orig })
	called := false
	triggerLogsRemote = func(string, string) (string, error) { called = true; return "", nil }

	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowAutomationTriggers) // the group header, not a trigger
	fp.Update(m, key('L'))
	if called {
		t.Error("L on a non-trigger row must not fetch trigger logs")
	}
}
