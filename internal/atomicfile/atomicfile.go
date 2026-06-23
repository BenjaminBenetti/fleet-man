// Package atomicfile writes a file atomically: a temp file in the same
// directory, then a rename over the destination. It is the one home for the
// temp+rename dance several ~/.fleet writers share (state.json, config.json,
// armada.json, the mcp.* discovery/secret files, gateway_session.json) so a
// concurrent reader — notably the hourly backup loop, which reads these files
// without any lock — never observes a torn write.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write writes data to path via a temp file in the SAME directory followed by a
// rename. Because rename is atomic on POSIX, a concurrent reader always sees
// either the old file or the new one in full — never the truncated/partial
// state a plain os.WriteFile (which opens O_TRUNC and then writes) can expose.
// On a crash mid-write the destination is never corrupt; at worst a stale temp
// file is left in the directory (callers/janitors reap it). The rename also
// (re)applies perm to any pre-existing file.
//
// This concerns reader atomicity, NOT fsync durability: like the rest of the
// codebase it does not fsync, so a power loss immediately after rename may still
// revert to the previous contents on some filesystems — it just can't leave a
// half-written one. The parent directory must already exist.
func Write(path string, data []byte, perm os.FileMode) error {
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
