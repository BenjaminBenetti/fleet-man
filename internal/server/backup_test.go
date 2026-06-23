package server

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// writeFleetState lays down a populated ~/.fleet under a temp HOME and returns
// the home dir. Only a subset of backupSources may be created so tests can
// exercise the "some files missing" path.
func writeFleetState(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .fleet: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return home
}

// readArchive unpacks a .tar.xz into name->content, proving the output is a
// standard xz/tar stream (the same thing `tar -xJf` would read).
func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		t.Fatalf("xz reader: %v", err)
	}
	tr := tar.NewReader(xr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(data)
	}
	return out
}

func TestWriteBackupRoundTrip(t *testing.T) {
	writeFleetState(t, map[string]string{
		"config.json":          `{"cfg":1}`,
		"gateway_session.json": `{"gw":"s"}`,
		"mcp.env":              "export FOO=1",
		"mcp.port":             "6012",
		"mcp.token":            "secret-token",
		"state.json":           `{"fleets":{}}`,
	})

	now := time.Date(2026, 6, 23, 14, 5, 0, 0, time.UTC)
	path, n, err := writeBackup(now)
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}
	if n != 6 {
		t.Fatalf("captured %d files, want 6", n)
	}
	wantPath := filepath.Join(backupBaseDir(), "2026-06-23", "14.tar.xz")
	if path != wantPath {
		t.Fatalf("archive path = %s, want %s", path, wantPath)
	}

	// The archive is owner-only because it embeds mcp.token.
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat archive: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("archive perms = %o, want 600", perm)
	}

	got := readArchive(t, path)
	want := map[string]string{
		"config.json":          `{"cfg":1}`,
		"gateway_session.json": `{"gw":"s"}`,
		"mcp.env":              "export FOO=1",
		"mcp.port":             "6012",
		"mcp.token":            "secret-token",
		"state.json":           `{"fleets":{}}`,
	}
	if len(got) != len(want) {
		t.Fatalf("archive has %d entries, want %d: %v", len(got), len(want), keys(got))
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("entry %s = %q, want %q", name, got[name], content)
		}
	}
}

func TestWriteBackupSkipsMissingFiles(t *testing.T) {
	// Only two of the six sources exist; the archive should hold exactly those.
	writeFleetState(t, map[string]string{
		"config.json": "{}",
		"state.json":  "{}",
	})
	now := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	path, n, err := writeBackup(now)
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}
	if n != 2 {
		t.Fatalf("captured %d files, want 2", n)
	}
	got := keys(readArchive(t, path))
	sort.Strings(got)
	want := []string{"config.json", "state.json"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("archive entries = %v, want %v", got, want)
	}
}

func TestWriteBackupNoSourcesWritesNothing(t *testing.T) {
	writeFleetState(t, nil) // ~/.fleet exists but is empty
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	path, n, err := writeBackup(now)
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}
	if n != 0 || path != "" {
		t.Fatalf("expected no archive, got path=%q n=%d", path, n)
	}
	if _, err := os.Stat(backupBaseDir()); !os.IsNotExist(err) {
		t.Fatalf("backup dir should not exist when nothing was captured")
	}
}

func TestWriteBackupOverwritesSameHour(t *testing.T) {
	home := writeFleetState(t, map[string]string{"state.json": "v1"})
	now := time.Date(2026, 6, 23, 14, 5, 0, 0, time.UTC)
	if _, _, err := writeBackup(now); err != nil {
		t.Fatalf("first writeBackup: %v", err)
	}
	// Mutate state and snapshot again later in the SAME hour: same path, new content.
	if err := os.WriteFile(filepath.Join(home, ".fleet", "state.json"), []byte("v2"), 0o600); err != nil {
		t.Fatalf("rewrite state: %v", err)
	}
	path, _, err := writeBackup(now.Add(20 * time.Minute))
	if err != nil {
		t.Fatalf("second writeBackup: %v", err)
	}
	if got := readArchive(t, path)["state.json"]; got != "v2" {
		t.Fatalf("re-snapshot state.json = %q, want v2", got)
	}
	// Exactly one archive for the hour.
	entries, _ := os.ReadDir(filepath.Join(backupBaseDir(), "2026-06-23"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 archive for the hour, got %d", len(entries))
	}
}

func TestPruneBackupsRetention(t *testing.T) {
	writeFleetState(t, nil)
	// Pin the window for the test and restore it after, so test order can't leak
	// a mutated global to other tests.
	defer func(prev time.Duration) { backupRetention = prev }(backupRetention)
	backupRetention = 30 * 24 * time.Hour

	base := backupBaseDir()
	// An expired day (well past 30 days), a boundary day with one expired and one
	// kept hour, and a fresh day.
	mkArchive := func(day, hour string) {
		d := filepath.Join(base, day)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(d, hour+".tar.xz"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write archive: %v", err)
		}
	}
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) // cutoff = 2026-05-24 12:00
	mkArchive("2026-04-01", "10")                        // expired (whole day gone)
	mkArchive("2026-05-24", "08")                        // expired (before 12:00 cutoff)
	mkArchive("2026-05-24", "15")                        // kept (after cutoff, same day)
	mkArchive("2026-06-23", "11")                        // kept
	// An expired day that ALSO holds a non-archive file: the archive is pruned but
	// the day dir must survive (it isn't empty) and the foreign file is untouched.
	mkArchive("2026-03-15", "09")
	if err := os.WriteFile(filepath.Join(base, "2026-03-15", "README"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write day-dir foreign file: %v", err)
	}
	// A non-backup directory at the root should be left strictly alone.
	if err := os.MkdirAll(filepath.Join(base, "notes"), 0o700); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "notes", "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	removed, err := pruneBackups(now)
	if err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d archives, want 3", removed)
	}
	// Fully-expired day directory is gone; partially-expired day survives with its
	// kept hour; fresh day survives; foreign dir untouched.
	assertAbsent(t, filepath.Join(base, "2026-04-01"))
	assertAbsent(t, filepath.Join(base, "2026-05-24", "08.tar.xz"))
	assertPresent(t, filepath.Join(base, "2026-05-24", "15.tar.xz"))
	assertPresent(t, filepath.Join(base, "2026-06-23", "11.tar.xz"))
	assertPresent(t, filepath.Join(base, "notes", "keep.txt"))
	// Expired archive removed, but its day dir + foreign file remain.
	assertAbsent(t, filepath.Join(base, "2026-03-15", "09.tar.xz"))
	assertPresent(t, filepath.Join(base, "2026-03-15", "README"))
}

func TestPruneBackupsSweepsOrphanTempFiles(t *testing.T) {
	writeFleetState(t, nil)
	base := backupBaseDir()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	// A fresh day dir whose only content is an orphaned temp file (a snapshot
	// crashed before its rename): the temp file is swept and the now-empty day dir
	// removed — it isn't counted as a pruned archive.
	orphanDay := filepath.Join(base, "2026-06-23")
	if err := os.MkdirAll(orphanDay, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDay, ".backup-abc123.tar.xz.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	// A second day with a valid archive AND an orphan temp: temp swept, archive kept.
	keepDay := filepath.Join(base, "2026-06-22")
	if err := os.MkdirAll(keepDay, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keepDay, "10.tar.xz"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keepDay, ".backup-def456.tar.xz.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	removed, err := pruneBackups(now)
	if err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d, want 0 (temp sweeps are not counted as pruned archives)", removed)
	}
	assertAbsent(t, orphanDay) // emptied by the sweep, then removed
	assertAbsent(t, filepath.Join(keepDay, ".backup-def456.tar.xz.tmp"))
	assertPresent(t, filepath.Join(keepDay, "10.tar.xz"))
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone, err=%v", path, err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
}
