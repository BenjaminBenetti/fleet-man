package server

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/ulikunitz/xz"
)

// backup.go is the server-side state backup loop. fleetd periodically snapshots
// the handful of files that constitute its own durable state (~/.fleet/*.json
// and the mcp.* discovery/secret files) into a single compressed archive, so a
// botched write, an errant `fleet destroy`, or a corrupted state.json can be
// recovered from a recent point in time.
//
// Archives are written to ~/.fleet/backup/<date>/<hour>.tar.xz — one per clock
// hour, bucketed by day. xz is used (the most-compressing of the common
// formats) but via the pure-Go writer, so the daemon needs no `xz` binary on
// the host; the result still unpacks anywhere with the standard tools
// (`tar -xJf <archive>`). Keying on <date>/<hour> makes the write idempotent:
// a daemon restart within the same hour simply rewrites that hour's archive.
//
// Restore is intentionally out of scope (a later change): this loop only
// captures and prunes. Like the automation scheduler it runs unconditionally —
// backups must happen whether or not a TUI is connected — and stops on
// shutdown via the context it is launched with.
var (
	// backupInterval is how often a snapshot is taken. The spec is hourly;
	// overridable via FLEET_BACKUP_INTERVAL (a Go duration) so a test can drive
	// the loop in milliseconds instead of an hour.
	backupInterval = envDurationDefault("FLEET_BACKUP_INTERVAL", time.Hour)
	// backupRetention bounds how far back archives are kept ("total max
	// retention of 1 month"). Anything older than now-retention is pruned after
	// each snapshot. Overridable via FLEET_BACKUP_RETENTION for tests.
	backupRetention = envDurationDefault("FLEET_BACKUP_RETENTION", 30*24*time.Hour)
)

// backupSources returns the absolute paths of the ~/.fleet files captured in
// each snapshot. Resolved per call (not cached) because the paths derive from
// $HOME, which tests rebind. Missing files are skipped at archive time, so a
// pristine ~/.fleet that has not yet grown all of these is fine.
func backupSources() []string {
	return []string{
		state.ConfigPath(),              // config.json
		fleetpaths.GatewaySessionPath(), // gateway_session.json
		fleetpaths.McpEnvPath(),         // mcp.env
		fleetpaths.McpPortPath(),        // mcp.port
		fleetpaths.McpTokenPath(),       // mcp.token
		state.StatePath(),               // state.json
	}
}

// backupBaseDir is the root of the backup tree (~/.fleet/backup). Server-only,
// so it lives here rather than in the client-shared fleetpaths package.
func backupBaseDir() string {
	return filepath.Join(fleetpaths.Dir(), "backup")
}

// runBackupLoop snapshots fleetd's state on backupInterval and prunes archives
// past the retention window. It returns when ctx is cancelled. It snapshots once
// immediately so a daemon that lives less than a full interval still leaves a
// backup; that first write just (re)fills the current hour's archive, which is
// harmless since the path is hour-keyed.
func (s *service) runBackupLoop(ctx context.Context) {
	ticker := time.NewTicker(backupInterval)
	defer ticker.Stop()
	for {
		backupTick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// backupTick performs one snapshot + prune. Both steps are best-effort: a
// failure is logged and the loop continues, since a backup miss must never take
// the daemon down. Split from the loop so tests can drive a single tick.
func backupTick(now time.Time) {
	switch path, n, err := writeBackup(now); {
	case err != nil:
		flog.Warn("backup: snapshot failed", "err", err)
	case n > 0:
		flog.Info("backup: wrote snapshot", "path", path, "files", n)
	}
	if removed, err := pruneBackups(now); err != nil {
		flog.Warn("backup: prune failed", "err", err)
	} else if removed > 0 {
		flog.Info("backup: pruned expired archives", "removed", removed)
	}
}

// writeBackup tars + xz-compresses the existing backupSources into
// ~/.fleet/backup/<date>/<hour>.tar.xz, returning the archive path and the
// number of files actually captured. It writes to a temp file in the same
// directory and renames into place, so a reader never sees a half-written
// archive. When none of the sources exist yet (a pristine ~/.fleet) it writes
// nothing and returns n==0.
func writeBackup(now time.Time) (string, int, error) {
	sources := backupSources()
	// Skip entirely if there is nothing to capture, rather than emit an empty
	// archive for the hour.
	present := sources[:0:0]
	for _, src := range sources {
		if info, err := os.Stat(src); err == nil && info.Mode().IsRegular() {
			present = append(present, src)
		}
	}
	if len(present) == 0 {
		return "", 0, nil
	}

	dir := filepath.Join(backupBaseDir(), now.Format("2006-01-02"))
	// 0700: the archive embeds mcp.token (a secret), so the whole backup tree is
	// owner-only, matching how ~/.fleet itself is created.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	finalPath := filepath.Join(dir, now.Format("15")+".tar.xz")

	tmp, err := os.CreateTemp(dir, ".backup-*.tar.xz.tmp")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	// Remove the temp file on any early return; a no-op once it is renamed away.
	defer func() {
		_ = os.Remove(tmpName)
	}()

	n, err := func() (int, error) {
		defer tmp.Close()
		xw, err := xz.NewWriter(tmp)
		if err != nil {
			return 0, err
		}
		tw := tar.NewWriter(xw)
		count := 0
		for _, src := range present {
			added, err := addFileToTar(tw, src)
			if err != nil {
				return 0, err
			}
			if added {
				count++
			}
		}
		// Close the tar then the xz writer in order so their trailers flush
		// before the rename; closing the file is left to the deferred Close.
		if err := tw.Close(); err != nil {
			return 0, err
		}
		if err := xw.Close(); err != nil {
			return 0, err
		}
		return count, nil
	}()
	if err != nil {
		return "", 0, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return "", 0, err
	}
	return finalPath, n, nil
}

// addFileToTar writes one regular file into tw under its base name (the archive
// is a flat bag of ~/.fleet files, not a directory tree). A file that vanished
// between the stat above and here is skipped rather than failing the whole
// snapshot.
//
// The header Size is taken from the bytes actually read, NOT from the stat: the
// sources are rewritten live by this same daemon (state.Save/SaveConfig do an
// in-place, non-atomic os.WriteFile of state.json/config.json), so a rewrite
// landing between stat and read would leave hdr.Size != len(data) and make
// tar.Writer fail the whole snapshot. Deriving Size from the read keeps the
// header self-consistent — at worst we capture the old-or-new file in full.
func addFileToTar(tw *tar.Writer, path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return false, err
	}
	hdr.Name = filepath.Base(path)
	hdr.Size = int64(len(data)) // must match the bytes written below
	if err := tw.WriteHeader(hdr); err != nil {
		return false, err
	}
	if _, err := tw.Write(data); err != nil {
		return false, err
	}
	return true, nil
}

// pruneBackups removes archives older than the retention window and any backup
// day-directory left empty afterwards, returning how many archive files it
// deleted. An archive's age is taken from its <date>/<hour> path (not its mtime)
// so the retention boundary is exact and independent of when the file was
// touched. Unrecognized entries (anything not matching <date>/<hour>.tar.xz) are
// left untouched.
func pruneBackups(now time.Time) (int, error) {
	base := backupBaseDir()
	days, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-backupRetention)
	removed := 0
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		dayStart, err := time.ParseInLocation("2006-01-02", day.Name(), now.Location())
		if err != nil {
			continue // not a backup day dir
		}
		dayPath := filepath.Join(base, day.Name())
		archives, err := os.ReadDir(dayPath)
		if err != nil {
			continue
		}
		for _, a := range archives {
			if a.IsDir() {
				continue
			}
			name := a.Name()
			// Reclaim a temp file orphaned by a snapshot that crashed before its
			// rename (the deferred cleanup never ran). Safe to delete here:
			// snapshots are serial and the current tick's writeBackup has already
			// finished by the time prune runs, so any *.tmp left is from a dead run.
			// Left behind, it would also keep an otherwise-empty day dir alive.
			if strings.HasPrefix(name, ".backup-") && strings.HasSuffix(name, ".tmp") {
				_ = os.Remove(filepath.Join(dayPath, name))
				continue
			}
			hourStr := strings.TrimSuffix(name, ".tar.xz")
			if hourStr == name {
				continue // not a .tar.xz archive
			}
			hour, err := strconv.Atoi(hourStr)
			if err != nil || hour < 0 || hour > 23 {
				continue
			}
			when := dayStart.Add(time.Duration(hour) * time.Hour)
			if when.Before(cutoff) {
				if err := os.Remove(filepath.Join(dayPath, name)); err == nil {
					removed++
				}
			}
		}
		// Drop the day directory once it holds no more archives.
		if rest, err := os.ReadDir(dayPath); err == nil && len(rest) == 0 {
			_ = os.Remove(dayPath)
		}
	}
	return removed, nil
}
