package server

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	// This set is exactly the files issue #205 calls out — fleetd's own durable
	// state. armada.json lives in ~/.fleet too (and is now an atomicfile.Write
	// writer), but is deliberately NOT here: it is the CLIENT's registry of
	// remote fleetd connections to switch between, not this daemon's state, so it
	// is out of fleetd's self-backup scope. Don't add it without revisiting that.
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
	present := make([]string, 0, len(sources))
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
	if n == 0 {
		// Every source vanished between the presence scan and the read. Honor the
		// "writes nothing rather than an empty archive" contract: drop the temp
		// file FIRST (the deferred Remove only fires on return, after the dir
		// removals below), then drop the day dir and the backup root if this call
		// just created them (each Remove no-ops when other entries still live
		// there, e.g. earlier-hour archives or other days).
		_ = os.Remove(tmpName)
		_ = os.Remove(dir)
		_ = os.Remove(backupBaseDir())
		return "", 0, nil
	}
	// The temp file is already 0600 (os.CreateTemp) and rename preserves it, so
	// the archive lands 0600 to protect the embedded mcp.token.
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
// The header Size is taken from the bytes actually read, NOT from the stat, so
// the header always matches the archived bytes even if a source changes size
// between the stat and the read — otherwise tar.Writer would fail the whole
// snapshot (ErrWriteTooLong / "missed writing N bytes"). The captured CONTENTS
// are kept whole by the writers: every backup source is now written via
// atomicfile.Write (temp+rename), so a concurrent rewrite yields the old or new
// file in full, never a torn one.
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

// parseArchiveHour reports whether name is a "<hour>.tar.xz" backup archive
// (hour 0–23, as written by now.Format("15")), returning the parsed hour and
// its original 2-digit string. It is the single definition of "what counts as
// an archive" shared by pruneBackups and listBackupArchives, so the two can't
// drift on it.
func parseArchiveHour(name string) (hourStr string, hour int, ok bool) {
	hourStr = strings.TrimSuffix(name, ".tar.xz")
	if hourStr == name {
		return "", 0, false // no .tar.xz suffix
	}
	h, err := strconv.Atoi(hourStr)
	if err != nil || h < 0 || h > 23 {
		return "", 0, false
	}
	return hourStr, h, true
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
			_, hour, ok := parseArchiveHour(name)
			if !ok {
				continue // not a .tar.xz archive
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

// --- restore documentation (MCP tool) ---

// fleet_restore_backup is intentionally a DOCUMENTATION-ONLY tool. fleetd owns
// ~/.fleet and rewrites these files live, so a "restore" that the daemon
// performed on itself would race its own writers; the only safe restore is to
// stop the daemon and unpack an archive by hand. So this tool tells the calling
// agent (fleet-admiral or similar) exactly where the archives are, what each one
// holds, which ones exist, and the step-by-step manual procedure — and performs
// no mutation itself.

// restoreBackupArchive is one discovered backup archive, newest listed first.
type restoreBackupArchive struct {
	Path string `json:"path"` // absolute path to the .tar.xz
	Date string `json:"date"` // <date> bucket, e.g. "2026-06-23"
	Hour string `json:"hour"` // <hour> bucket, e.g. "14"
}

// restoreBackupFile documents one file captured in every archive.
type restoreBackupFile struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// RestoreBackupOutput is the documentation returned by fleet_restore_backup.
type RestoreBackupOutput struct {
	Summary     string                 `json:"summary"`
	BackupDir   string                 `json:"backup_dir"`   // absolute ~/.fleet/backup
	RestoreInto string                 `json:"restore_into"` // absolute ~/.fleet
	PathLayout  string                 `json:"path_layout"`
	Compression string                 `json:"compression"`
	Contents    []restoreBackupFile    `json:"contents"`
	Available   []restoreBackupArchive `json:"available"`        // newest first; empty if none yet
	Latest      string                 `json:"latest,omitempty"` // path of the newest archive
	Procedure   []string               `json:"procedure"`        // ordered manual restore steps
	Warning     string                 `json:"warning"`
}

// listBackupArchives walks ~/.fleet/backup and returns every <date>/<hour>.tar.xz
// archive, newest first. Best-effort: unreadable or malformed entries are
// skipped, and a missing backup dir simply yields an empty slice.
func listBackupArchives() []restoreBackupArchive {
	base := backupBaseDir()
	days, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []restoreBackupArchive
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		if _, err := time.Parse("2006-01-02", day.Name()); err != nil {
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
			hourStr, _, ok := parseArchiveHour(a.Name())
			if !ok {
				continue
			}
			out = append(out, restoreBackupArchive{
				Path: filepath.Join(dayPath, a.Name()),
				Date: day.Name(),
				Hour: hourStr,
			})
		}
	}
	// Newest first: the path embeds zero-padded date then hour, so a reverse
	// lexical sort on Path orders by time without parsing.
	sort.Slice(out, func(i, j int) bool { return out[i].Path > out[j].Path })
	return out
}

// mcpRestoreBackup returns the restore documentation. Read-only: it scans the
// backup tree to list what exists but changes nothing.
func (s *service) mcpRestoreBackup(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, RestoreBackupOutput, error) {
	fleetDir := fleetpaths.Dir()
	archives := listBackupArchives()
	latest := ""
	if len(archives) > 0 {
		latest = archives[0].Path
	}

	out := RestoreBackupOutput{
		Summary:     "fleetd snapshots its own state hourly. To restore one, stop every fleet process (the daemon especially) and unpack the chosen archive over " + fleetDir + ". This tool only documents that procedure — it restores nothing itself.",
		BackupDir:   backupBaseDir(),
		RestoreInto: fleetDir,
		PathLayout:  "<backup_dir>/<date>/<hour>.tar.xz — one archive per clock hour (e.g. 2026-06-23/14.tar.xz), kept ~30 days.",
		Compression: "xz; each archive is a flat tar of the files below (base names, no directory tree), so it unpacks straight into the settings dir with `tar -xJf <archive> -C " + fleetDir + "`.",
		Contents: []restoreBackupFile{
			{Name: "config.json", Description: "user/global fleet configuration (settings, backends, remote-MCP/gateway config)."},
			{Name: "state.json", Description: "the fleet & instance registry — fleetd's core durable state."},
			{Name: "gateway_session.json", Description: "sticky remote-MCP gateway session (session id + public URL)."},
			{Name: "mcp.env", Description: "sourceable shell exports for the local MCP endpoint (FLEET_MCP_URL/PORT/TOKEN)."},
			{Name: "mcp.port", Description: "TCP port the local MCP HTTP server bound to."},
			{Name: "mcp.token", Description: "bearer token for the local MCP HTTP server — SECRET; treat the archive as sensitive."},
		},
		Available: archives,
		Latest:    latest,
		Procedure: []string{
			"1. Stop every running fleet process so nothing rewrites " + fleetDir + " mid-restore — the daemon (`fleet server` / fleetd) above all, since it owns these files and keeps snapshotting them. Run `pkill -f 'fleet server'`, then `pkill -x fleet` for any client/TUI, and confirm none remain with `pgrep -af fleet` (an empty result).",
			"2. Pick the archive to restore from `available` (newest first); `latest` is the most recent. Each is an absolute path to a <date>/<hour>.tar.xz.",
			"3. (Recommended) Snapshot the current settings dir before overwriting, e.g. `cp -a " + fleetDir + " " + fleetDir + ".pre-restore`.",
			"4. Unpack the archive over the settings dir: `tar -xJf <archive> -C " + fleetDir + "`. The archive holds base names only, so each file lands back at its original ~/.fleet/<file> path, overwriting the current copy.",
			"5. Restart the daemon — run any `fleet` command (it auto-spawns fleetd) or start `fleet server` directly — then verify with the `fleet_version` and `fleet_status` tools.",
		},
		Warning: "Stop fleetd BEFORE unpacking. It rewrites these files continuously and snapshots hourly, so restoring under a live daemon can corrupt state.json or have your restore immediately overwritten. The archives contain mcp.token (a secret) — handle them accordingly.",
	}
	if len(archives) == 0 {
		out.Summary = "No backups exist yet under " + out.BackupDir + " (the daemon writes the first within an hour of starting). " + out.Summary
	}
	return nil, out, nil
}
