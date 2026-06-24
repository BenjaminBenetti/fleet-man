package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// logrotate.go bounds the size of the shared event log. ~/.fleet/fleet.log is
// the append-only trail every fleet process writes to (see internal/flog), and
// nothing ever trims it, so on a long-lived host it grows without bound. This
// loop cuts it once a day: at 3am local time it copies the current contents to
// ~/.fleet/logs/fleetd/<date>.log, truncates the live log back to empty, and
// drops dated logs past the retention window (the last 100 days).
//
// Why copytruncate and not a rename. fleet.log is appended to with O_APPEND by
// EVERY fleet process at once — the long-lived daemon and TUI, one-shot CLIs,
// and the detached _create/_clone children — each holding its own file
// descriptor to the same inode for its whole lifetime, and flog opens that
// descriptor exactly once with no reopen path. Renaming the file would leave all
// those descriptors pointing at the renamed inode, so the daemon and TUI would
// keep writing into the rotated file while the fresh fleet.log collected only
// records from processes started afterwards. Truncating the inode in place keeps
// every open descriptor valid: because all writers use O_APPEND, their next
// write lands at the new end-of-file (offset 0) with no reopen and no
// cross-process coordination. The cost is copytruncate's well-known race —
// records appended in the brief window between the copy reaching EOF and the
// truncate are dropped — which is acceptable because flog is explicitly
// best-effort (swallow-and-continue).
//
// Like the backup loop it runs unconditionally (rotation must happen whether or
// not a TUI is connected) and stops on shutdown via the context it is launched
// with. The schedule and date naming use LOCAL time to match "3am system time";
// daily buckets are immune to the DST double-hour that pushes the backup loop to
// UTC.
var (
	// logRotateHour is the local hour-of-day (0–23) at which fleet.log is cut.
	// The spec is 3am; overridable via FLEET_LOG_ROTATE_HOUR so a test can target
	// the current hour. Clamped to a real hour so a fat-fingered override (e.g.
	// 25) can't make the "before the rotation hour" gate unsatisfiable and turn
	// every check into a rotation.
	logRotateHour = min(max(envIntDefault("FLEET_LOG_ROTATE_HOUR", 3), 0), 23)
	// logRotateKeepDays is how many of the most recent daily logs to keep (today
	// inclusive). After each rotation check, logs older than that window are
	// pruned. Overridable via FLEET_LOG_ROTATE_KEEP_DAYS for tests.
	logRotateKeepDays = envIntDefault("FLEET_LOG_ROTATE_KEEP_DAYS", 100)
	// logRotateInterval is how often the loop checks whether a rotation is due.
	// A daily cut needs no fine granularity; ~15m lands the rotation within a
	// quarter hour of 3am while keeping the check cheap. Overridable via
	// FLEET_LOG_ROTATE_INTERVAL (a Go duration) so a test can drive the loop fast.
	logRotateInterval = envDurationDefault("FLEET_LOG_ROTATE_INTERVAL", 15*time.Minute)
)

// envIntDefault returns the non-negative integer parsed from the named env var,
// or def when it is unset, blank, or unparseable. Like envDurationDefault these
// knobs exist only so tests can retarget the rotation hour / retention; the
// daemon inherits the spawning client's environment.
func envIntDefault(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// fleetdLogDir is the directory holding the rotated daily logs
// (~/.fleet/logs/fleetd). Server-only, so it lives here rather than in the
// client-shared fleetpaths package. The parent ~/.fleet/logs already holds the
// per-instance creation logs.
func fleetdLogDir() string {
	return filepath.Join(fleetpaths.Dir(), "logs", "fleetd")
}

// runLogRotateLoop rotates fleet.log when due and prunes expired dated logs on
// logRotateInterval. It returns when ctx is cancelled. It checks once
// immediately so a daemon that comes up after 3am on a day it has not yet
// rotated cuts the (possibly long-overgrown) log promptly instead of waiting a
// full interval.
func (s *service) runLogRotateLoop(ctx context.Context) {
	ticker := time.NewTicker(logRotateInterval)
	defer ticker.Stop()
	for {
		rotateLogTick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// rotateLogTick performs one rotation-if-due plus a prune. Both steps are
// best-effort: a failure is logged and the loop continues, since a rotation miss
// must never take the daemon down. Split from the loop so tests can drive a
// single tick. The "rotated" info line lands as the first record of the freshly
// truncated fleet.log, so each cut is self-marking.
func rotateLogTick(now time.Time) {
	switch path, rotated, err := rotateLogIfDue(now); {
	case err != nil:
		flog.Warn("logrotate: rotation failed", "err", err)
	case rotated:
		flog.Info("logrotate: rotated fleet.log", "path", path)
	}
	if removed, err := pruneRotatedLogs(now); err != nil {
		flog.Warn("logrotate: prune failed", "err", err)
	} else if removed > 0 {
		flog.Info("logrotate: pruned expired logs", "removed", removed)
	}
}

// rotateLogIfDue cuts fleet.log into ~/.fleet/logs/fleetd/<date>.log when a
// rotation is due, returning the dated path and whether it rotated. A rotation
// is due once it is at or past logRotateHour local time and today's dated log
// does not yet exist; that filesystem-derived "already cut today" test makes the
// rotation idempotent and gives free catch-up across daemon restarts without any
// in-memory bookkeeping. An empty or missing fleet.log is left alone so quiet
// days produce no empty dated files.
func rotateLogIfDue(now time.Time) (string, bool, error) {
	if now.Hour() < logRotateHour {
		return "", false, nil // not yet the rotation hour today
	}

	src := flog.Path()
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return "", false, nil // nothing logged yet
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", false, nil // nothing to rotate
	}

	// Name the dated log by the rotation date (today). The file holds everything
	// accumulated since the previous cut, so under catch-up its name marks WHEN it
	// was cut rather than mislabeling a multi-day backlog as a single prior day.
	dir := fleetdLogDir()
	dest := filepath.Join(dir, now.Format("2006-01-02")+".log")
	if _, err := os.Stat(dest); err == nil {
		return "", false, nil // already rotated today
	}

	// 0755 to match the existing ~/.fleet/logs directory; the event log carries no
	// secrets (commands, names, errors), unlike the 0700 backup tree.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	if err := copyTruncate(src, dest); err != nil {
		return "", false, err
	}
	return dest, true, nil
}

// copyTruncate copies src's current contents to dest (via a temp file renamed
// into place, so a reader never sees a half-written dated log) and then
// truncates src to zero. See the file header for why this is a copytruncate
// rather than a rename. The copy is persisted BEFORE the truncate so a failure
// mid-rotation never loses data — fleet.log stays intact until its rotated copy
// is safely on disk.
func copyTruncate(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".rotate-*.log.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Remove the temp file on any early return; a no-op once it is renamed away.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Match fleet.log's own 0644 so the rotated copy is as readable as the live
	// file (os.CreateTemp makes it 0600).
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	return os.Truncate(src, 0)
}

// parseRotatedLogDate reports whether name is a "<date>.log" rotated log,
// returning the date parsed in local time (midnight) to line up with the
// local-time rotation/prune clock. It is the single definition of "what counts
// as a rotated log" shared by pruneRotatedLogs, so the prune can't drift from
// the rotation's naming.
func parseRotatedLogDate(name string) (time.Time, bool) {
	s := strings.TrimSuffix(name, ".log")
	if s == name {
		return time.Time{}, false // no .log suffix
	}
	day, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// pruneRotatedLogs removes dated logs more than logRotateKeepDays days old,
// returning how many it deleted. A log's age is taken from its <date> filename
// (not its mtime) so the retention boundary is exact and independent of when the
// file was last touched, mirroring how the backup loop prunes by its <date>
// path. Entries that are not <date>.log files are left untouched.
func pruneRotatedLogs(now time.Time) (int, error) {
	dir := fleetdLogDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// Keep the most recent logRotateKeepDays days, today inclusive: the window
	// starts keepDays-1 days before today, so a log dated before that start is
	// outside the window and removed. With the default 100 that retains today plus
	// the 99 days behind it — 100 daily logs.
	cutoff := startOfDay(now).AddDate(0, 0, -(logRotateKeepDays - 1))
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Reclaim a temp file orphaned by a rotation that died before its rename
		// (the deferred cleanup never ran). Safe here: rotation is serial and the
		// current tick's copyTruncate has already finished by the time prune runs,
		// so any .rotate-*.log.tmp left behind is from a dead run. Mirrors how
		// pruneBackups sweeps its own orphaned .backup-*.tmp files; without it these
		// would accumulate forever, since the date filter below skips them.
		if strings.HasPrefix(name, ".rotate-") && strings.HasSuffix(name, ".log.tmp") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		day, ok := parseRotatedLogDate(name)
		if !ok {
			continue
		}
		if day.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// startOfDay returns midnight of t's calendar day in t's own location.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
