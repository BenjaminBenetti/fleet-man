//go:build !linux

package sharesync

import (
	"errors"
	"log/slog"
)

// watchDir has no watcher here: macOS's FSEvents needs cgo and kqueue an fd
// per file, so the side polls (a scan of an unchanged tree only Lstats).
func watchDir(string, func(string) bool, *slog.Logger) (watcher, error) {
	return nil, errors.ErrUnsupported
}
