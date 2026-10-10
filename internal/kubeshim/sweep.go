package kubeshim

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// dirPrefix starts every shim directory's name.
const dirPrefix = "fleet-kubeshim-"

// SweepStale removes shim directories older than maxAge that this process's
// user owns: the ones a daemon that died mid-build or mid-hook left behind,
// which still hold the capture's environment values. It looks where NewDir
// makes them (the temp dir and /tmp), and returns how many it removed. A
// directory another user owns, or one young enough to belong to a run in
// progress, is left alone.
func SweepStale(maxAge time.Duration, now time.Time) int {
	return sweepStaleIn([]string{os.TempDir(), "/tmp"}, maxAge, now)
}

func sweepStaleIn(bases []string, maxAge time.Duration, now time.Time) int {
	removed := 0
	seen := map[string]bool{}
	for _, base := range bases {
		if seen[base] {
			continue
		}
		seen[base] = true
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), dirPrefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < maxAge || !ownedByMe(info) {
				continue
			}
			if os.RemoveAll(filepath.Join(base, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
