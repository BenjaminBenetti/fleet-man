package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path durably: it writes a temp file in the
// SAME directory, then renames it over path. Because rename is atomic on POSIX,
// a concurrent reader always observes either the old file or the new one in
// full — never a truncated/partial write (which a plain os.WriteFile exposes,
// since it opens O_TRUNC and then writes). A crash mid-write leaves only a
// stale temp file, never a corrupt destination. The rename also (re)applies
// perm to any pre-existing file.
//
// This is the single home for the temp+rename dance the state writers share
// (saveLocked, SaveConfig, SaveArmada). The parent directory must already
// exist — callers MkdirAll it before serializing.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing file: %w", err)
	}
	return nil
}
