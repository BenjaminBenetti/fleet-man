package sharesync

import "log/slog"

// watcher tells a side which paths of a root's folder changed. A watcher
// only ever triggers scans, which do their own safe walking, so a path it
// gets wrong (a folder moved, a symlink swapped in) costs a scan, nothing
// more.
type watcher interface {
	// ready is signalled when changed paths are waiting.
	ready() <-chan struct{}
	// drain returns the paths changed since the last drain, relative to the
	// folder; "." is all of it (the watcher lost events).
	drain() []string
	// limited reports that the watcher could not watch every folder (the
	// system's watch limit), so the folder must be polled.
	limited() bool
	close() error
}

// newWatcher watches the folder dir and every folder below it that skip
// does not name (relative, slash-separated); errors.ErrUnsupported where the
// system has no watcher, and the side polls instead. A variable for tests.
var newWatcher = func(dir string, skip func(rel string) bool, log *slog.Logger) (watcher, error) {
	return watchDir(dir, skip, log)
}
