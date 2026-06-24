package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// seedFleetLog lays down ~/.fleet/fleet.log with content under a temp HOME and
// returns the home dir. An empty content string still creates the file (size 0).
func seedFleetLog(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".fleet"), 0o755); err != nil {
		t.Fatalf("mkdir .fleet: %v", err)
	}
	if err := os.WriteFile(flog.Path(), []byte(content), 0o644); err != nil {
		t.Fatalf("write fleet.log: %v", err)
	}
	return home
}

// setRotateConfig overrides the rotation knobs (read once from env at package
// init) for the duration of a test, restoring them after.
func setRotateConfig(t *testing.T, hour, keepDays int) {
	t.Helper()
	t.Cleanup(func(h, k int) func() { return func() { logRotateHour, logRotateKeepDays = h, k } }(logRotateHour, logRotateKeepDays))
	logRotateHour, logRotateKeepDays = hour, keepDays
}

func TestRotateLogIfDueBeforeHour(t *testing.T) {
	seedFleetLog(t, "before-the-hour\n")
	setRotateConfig(t, 3, 100)

	// 02:59 local — not yet 3am, so nothing should happen.
	now := time.Date(2026, 6, 24, 2, 59, 0, 0, time.Local)
	path, rotated, err := rotateLogIfDue(now)
	if err != nil {
		t.Fatalf("rotateLogIfDue: %v", err)
	}
	if rotated || path != "" {
		t.Fatalf("rotated before the hour: rotated=%v path=%q", rotated, path)
	}
	if got := readFile(t, flog.Path()); got != "before-the-hour\n" {
		t.Fatalf("fleet.log changed: %q", got)
	}
	if _, err := os.Stat(fleetdLogDir()); !os.IsNotExist(err) {
		t.Fatalf("fleetd log dir should not exist yet: %v", err)
	}
}

func TestRotateLogIfDueRotatesAndTruncates(t *testing.T) {
	seedFleetLog(t, "day of records\n")
	setRotateConfig(t, 3, 100)

	now := time.Date(2026, 6, 24, 3, 5, 0, 0, time.Local)
	path, rotated, err := rotateLogIfDue(now)
	if err != nil {
		t.Fatalf("rotateLogIfDue: %v", err)
	}
	if !rotated {
		t.Fatal("expected rotation")
	}
	wantPath := filepath.Join(fleetdLogDir(), "2026-06-24.log")
	if path != wantPath {
		t.Fatalf("dated path = %q, want %q", path, wantPath)
	}
	if got := readFile(t, wantPath); got != "day of records\n" {
		t.Fatalf("dated log content = %q", got)
	}
	// The live log is cut back to empty so it can be appended to afresh.
	if info, err := os.Stat(flog.Path()); err != nil || info.Size() != 0 {
		t.Fatalf("fleet.log not truncated: size=%v err=%v", sizeOf(info), err)
	}
	// The dated copy inherits fleet.log's 0644, not CreateTemp's 0600.
	if info, _ := os.Stat(wantPath); info.Mode().Perm() != 0o644 {
		t.Fatalf("dated log perm = %v, want 0644", info.Mode().Perm())
	}
}

func TestRotateLogIfDueIdempotentWithinDay(t *testing.T) {
	seedFleetLog(t, "first\n")
	setRotateConfig(t, 3, 100)

	now := time.Date(2026, 6, 24, 3, 5, 0, 0, time.Local)
	if _, rotated, err := rotateLogIfDue(now); err != nil || !rotated {
		t.Fatalf("first rotate: rotated=%v err=%v", rotated, err)
	}
	// New activity accrues, and a later check the same day runs.
	if err := os.WriteFile(flog.Path(), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("re-write fleet.log: %v", err)
	}
	later := time.Date(2026, 6, 24, 9, 0, 0, 0, time.Local)
	if path, rotated, err := rotateLogIfDue(later); err != nil || rotated || path != "" {
		t.Fatalf("second rotate should be a no-op: rotated=%v path=%q err=%v", rotated, path, err)
	}
	// The dated file keeps the FIRST cut and the live log keeps its new content —
	// today's log is never clobbered mid-day.
	if got := readFile(t, filepath.Join(fleetdLogDir(), "2026-06-24.log")); got != "first\n" {
		t.Fatalf("dated log overwritten: %q", got)
	}
	if got := readFile(t, flog.Path()); got != "second\n" {
		t.Fatalf("fleet.log clobbered: %q", got)
	}
}

func TestRotateLogIfDueSkipsEmptyAndMissing(t *testing.T) {
	setRotateConfig(t, 3, 100)
	now := time.Date(2026, 6, 24, 3, 5, 0, 0, time.Local)

	// Empty fleet.log → no empty dated file.
	seedFleetLog(t, "")
	if path, rotated, err := rotateLogIfDue(now); err != nil || rotated || path != "" {
		t.Fatalf("empty log rotated: rotated=%v path=%q err=%v", rotated, path, err)
	}
	if _, err := os.Stat(fleetdLogDir()); !os.IsNotExist(err) {
		t.Fatalf("dated dir created for empty log: %v", err)
	}

	// Missing fleet.log → no-op (remove the seeded one).
	if err := os.Remove(flog.Path()); err != nil {
		t.Fatalf("remove fleet.log: %v", err)
	}
	if path, rotated, err := rotateLogIfDue(now); err != nil || rotated || path != "" {
		t.Fatalf("missing log rotated: rotated=%v path=%q err=%v", rotated, path, err)
	}
}

// TestRotatePreservesSharedAppendFd is the core guarantee: a process holding an
// O_APPEND descriptor to fleet.log keeps writing into the SAME file across a
// rotation, because copytruncate keeps the inode. A rename would orphan the
// descriptor onto the rotated file.
func TestRotatePreservesSharedAppendFd(t *testing.T) {
	seedFleetLog(t, "")
	setRotateConfig(t, 3, 100)

	// Simulate a long-lived writer (the daemon/TUI) holding fleet.log open.
	w, err := os.OpenFile(flog.Path(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open fleet.log O_APPEND: %v", err)
	}
	defer w.Close()

	if _, err := w.WriteString("before-rotate\n"); err != nil {
		t.Fatalf("write before: %v", err)
	}
	now := time.Date(2026, 6, 24, 3, 5, 0, 0, time.Local)
	if _, rotated, err := rotateLogIfDue(now); err != nil || !rotated {
		t.Fatalf("rotate: rotated=%v err=%v", rotated, err)
	}
	// Same descriptor, post-rotation write — must land in the (now truncated)
	// fleet.log, NOT the rotated dated file.
	if _, err := w.WriteString("after-rotate\n"); err != nil {
		t.Fatalf("write after: %v", err)
	}

	if got := readFile(t, flog.Path()); got != "after-rotate\n" {
		t.Fatalf("live log after rotation = %q, want only the post-rotate line", got)
	}
	if got := readFile(t, filepath.Join(fleetdLogDir(), "2026-06-24.log")); got != "before-rotate\n" {
		t.Fatalf("dated log = %q, want the pre-rotate line", got)
	}
}

func TestPruneRotatedLogs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setRotateConfig(t, 3, 100)
	dir := fleetdLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Date(2026, 6, 24, 3, 0, 0, 0, time.Local)
	// dayLog writes a <date>.log for now-offset days.
	dayLog := func(daysAgo int) string {
		name := now.AddDate(0, 0, -daysAgo).Format("2006-01-02") + ".log"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return name
	}
	keep0 := dayLog(0)     // today
	keep99 := dayLog(99)   // oldest still inside the 100-day window
	drop100 := dayLog(100) // just outside the window — removed
	drop101 := dayLog(101) // over the limit — removed
	drop500 := dayLog(500) // well over — removed
	// A non-matching file must be left strictly alone.
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	// A crash-orphaned rotation temp must be reclaimed (but not counted as a
	// removed dated log).
	orphan := filepath.Join(dir, ".rotate-987654.log.tmp")
	if err := os.WriteFile(orphan, []byte("half-written"), 0o600); err != nil {
		t.Fatalf("write orphan tmp: %v", err)
	}

	removed, err := pruneRotatedLogs(now)
	if err != nil {
		t.Fatalf("pruneRotatedLogs: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	for _, name := range []string{keep0, keep99} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s should be kept: %v", name, err)
		}
	}
	for _, name := range []string{drop100, drop101, drop500} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed: %v", name, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-log file removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned rotate tmp not reclaimed: %v", err)
	}
}

func TestPruneRotatedLogsNoDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 6, 24, 3, 0, 0, 0, time.Local)
	if removed, err := pruneRotatedLogs(now); err != nil || removed != 0 {
		t.Fatalf("prune with no dir: removed=%d err=%v", removed, err)
	}
}

func TestParseRotatedLogDate(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"2026-06-24.log", true},
		{"2026-01-01.log", true},
		{"2026-06-24.log.tmp", false}, // temp file, not a finished log
		{"fleet.log", false},          // no date
		{"2026-13-40.log", false},     // impossible date
		{"notes.txt", false},          // not .log
		{".rotate-123.log.tmp", false},
	}
	for _, c := range cases {
		if _, ok := parseRotatedLogDate(c.name); ok != c.ok {
			t.Errorf("parseRotatedLogDate(%q) ok = %v, want %v", c.name, ok, c.ok)
		}
	}
}

func TestEnvIntDefault(t *testing.T) {
	const key = "FLEET_TEST_INT_KNOB"
	cases := []struct {
		val string
		set bool
		def int
		fn  int
	}{
		{"", false, 7, 7},   // unset → default
		{"  ", true, 7, 7},  // blank → default
		{"12", true, 7, 12}, // parsed
		{"0", true, 7, 0},   // zero is valid
		{"-3", true, 7, 7},  // negative rejected
		{"abc", true, 7, 7}, // unparseable → default
	}
	for _, c := range cases {
		if c.set {
			t.Setenv(key, c.val)
		} else {
			os.Unsetenv(key)
		}
		if got := envIntDefault(key, c.def); got != c.fn {
			t.Errorf("envIntDefault(%q set=%v) = %d, want %d", c.val, c.set, got, c.fn)
		}
	}
}

// --- small test helpers ---

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func sizeOf(info os.FileInfo) int64 {
	if info == nil {
		return -1
	}
	return info.Size()
}
