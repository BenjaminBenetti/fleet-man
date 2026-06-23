package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestLogTriggerEventWebhook writes a webhook firing and reads it back: the log
// must carry the raw request body plus a header naming the trigger.
func TestLogTriggerEventWebhook(t *testing.T) {
	isolateFleetDir(t)
	ev := &triggerEvent{
		kind:        fleet.TriggerWebhook,
		triggerName: "ci-hook",
		firedAt:     time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC),
		webhookName: "ci",
		body:        []byte(`{"action":"opened"}`),
	}
	logTriggerEvent("alpha", ev)

	logs, count, err := readTriggerLogs("alpha", "ci-hook")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	for _, want := range []string{"# trigger: ci-hook", "# type:    webhook", "# webhook: ci", `{"action":"opened"}`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
}

// TestLogTriggerEventSchedule records a schedule firing — its payload is the fire
// time (schedule triggers carry no external body).
func TestLogTriggerEventSchedule(t *testing.T) {
	isolateFleetDir(t)
	ev := &triggerEvent{
		kind:        fleet.TriggerSchedule,
		triggerName: "nightly",
		firedAt:     time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC),
	}
	logTriggerEvent("alpha", ev)

	logs, count, err := readTriggerLogs("alpha", "nightly")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if strings.Contains(logs, "# webhook:") {
		t.Errorf("schedule log must not carry a webhook line:\n%s", logs)
	}
	if !strings.Contains(logs, "2026-06-23T00:00:00Z") {
		t.Errorf("schedule log missing fire-time payload:\n%s", logs)
	}
}

// TestTriggerLogRotation proves the per-trigger history is capped: writing more
// than triggerEventLogKeep firings keeps only the newest ones, dropping the
// oldest.
func TestTriggerLogRotation(t *testing.T) {
	isolateFleetDir(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const extra = 5
	for i := 0; i < triggerEventLogKeep+extra; i++ {
		logTriggerEvent("alpha", &triggerEvent{
			kind:        fleet.TriggerSchedule,
			triggerName: "nightly",
			firedAt:     base.Add(time.Duration(i) * time.Second),
		})
	}

	dir := state.TriggerLogsDir("alpha", "nightly")
	names := eventLogNames(dir)
	if len(names) != triggerEventLogKeep {
		t.Fatalf("kept %d logs, want %d", len(names), triggerEventLogKeep)
	}
	// The oldest `extra` firings (seconds 0..extra-1) must have been pruned; the
	// first surviving log is the one at second `extra`.
	oldest := "event-" + base.Format("20060102T150405Z") + ".log"
	if _, err := os.Stat(filepath.Join(dir, oldest)); !os.IsNotExist(err) {
		t.Errorf("oldest log %s should have been pruned (err=%v)", oldest, err)
	}
	firstKept := "event-" + base.Add(extra*time.Second).Format("20060102T150405Z") + ".log"
	if names[0] != firstKept {
		t.Errorf("first surviving log = %s, want %s", names[0], firstKept)
	}
}

// TestUniqueTriggerLogPathNoOverwrite proves firings in the same second each get
// their own log file rather than clobbering one another, AND that the on-disk
// order matches firing order — the collision suffix must keep lexical filename
// order chronological (eventLogNames/prune/read all sort by name).
func TestUniqueTriggerLogPathNoOverwrite(t *testing.T) {
	isolateFleetDir(t)
	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	bodies := []string{"first-firing", "second-firing", "third-firing"}
	for _, body := range bodies {
		logTriggerEvent("alpha", &triggerEvent{
			kind: fleet.TriggerWebhook, triggerName: "burst", webhookName: "ci", firedAt: at, body: []byte(body),
		})
	}
	dir := state.TriggerLogsDir("alpha", "burst")
	if names := eventLogNames(dir); len(names) != 3 {
		t.Fatalf("same-second firings produced %d logs, want 3: %v", len(names), names)
	}
	logs, _, err := readTriggerLogs("alpha", "burst")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Each body must appear, in firing order.
	prev := -1
	for _, body := range bodies {
		idx := strings.Index(logs, body)
		if idx < 0 {
			t.Fatalf("body %q missing from logs:\n%s", body, logs)
		}
		if idx < prev {
			t.Fatalf("same-second logs out of firing order (%q before a prior firing):\n%s", body, logs)
		}
		prev = idx
	}
}

// TestTriggerLogPerms: the durable logs (which can hold webhook secrets) are
// owner-only — 0600 files in a 0700 trigger dir.
func TestTriggerLogPerms(t *testing.T) {
	isolateFleetDir(t)
	logTriggerEvent("alpha", &triggerEvent{kind: fleet.TriggerSchedule, triggerName: "nightly", firedAt: time.Now()})
	dir := state.TriggerLogsDir("alpha", "nightly")
	if fi, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("trigger log dir perm = %o, want 700", perm)
	}
	names := eventLogNames(dir)
	if len(names) != 1 {
		t.Fatalf("want 1 log, got %v", names)
	}
	if fi, err := os.Stat(filepath.Join(dir, names[0])); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("trigger log file perm = %o, want 600", perm)
	}
}

// TestReadTriggerLogsEmpty: a trigger that has never fired returns empty/0, not
// an error.
func TestReadTriggerLogsEmpty(t *testing.T) {
	isolateFleetDir(t)
	logs, count, err := readTriggerLogs("alpha", "never-fired")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if logs != "" || count != 0 {
		t.Fatalf("empty trigger returned logs=%q count=%d", logs, count)
	}
}

// TestTriggerLogsDirNoEscape: a crafted fleet/trigger name can't escape the logs
// tree (the sanitizer maps path separators / traversal to safe segments).
func TestTriggerLogsDirNoEscape(t *testing.T) {
	isolateFleetDir(t)
	logsRoot := filepath.Join(state.FleetDir(), "logs")
	dir := state.TriggerLogsDir("../../etc", "../../../tmp/evil")
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(logsRoot)) {
		t.Fatalf("trigger log dir escaped the logs tree: %s", dir)
	}
}

// TestTriggerLogsRPC drives the service handler end to end.
func TestTriggerLogsRPC(t *testing.T) {
	isolateFleetDir(t)
	s := newService()

	// Missing args are rejected.
	if _, err := s.TriggerLogs(context.Background(), &fleetgrpc.TriggerLogsRequest{Fleet: "alpha"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty trigger: err = %v, want InvalidArgument", err)
	}

	// No firings yet → empty reply, no error.
	reply, err := s.TriggerLogs(context.Background(), &fleetgrpc.TriggerLogsRequest{Fleet: "alpha", Trigger: "nightly"})
	if err != nil {
		t.Fatalf("logs (empty): %v", err)
	}
	if reply.GetCount() != 0 || reply.GetLogs() != "" {
		t.Fatalf("empty reply = %+v", reply)
	}

	// After a firing, the reply carries it.
	logTriggerEvent("alpha", &triggerEvent{
		kind:        fleet.TriggerWebhook,
		triggerName: "nightly",
		firedAt:     time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		webhookName: "ci",
		body:        []byte("payload-bytes"),
	})
	reply, err = s.TriggerLogs(context.Background(), &fleetgrpc.TriggerLogsRequest{Fleet: "alpha", Trigger: "nightly"})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if reply.GetCount() != 1 || !strings.Contains(reply.GetLogs(), "payload-bytes") {
		t.Fatalf("reply = %+v", reply)
	}
}
