//go:build linux

package sharesync

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// watchMask is what an inotify watch reports: every change to a folder's
// entries and to the folder itself, never following a symlink.
const watchMask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_DELETE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

// inotifyWatcher watches a folder tree with one inotify watch per folder.
type inotifyWatcher struct {
	dir  string
	skip func(string) bool
	log  *slog.Logger
	f    *os.File
	fd   int
	sig  chan struct{}

	mu    sync.Mutex
	wds   map[int32]string
	paths map[string]bool
	full  bool
	limit bool
}

// watchDir watches dir with inotify.
func watchDir(dir string, skip func(string) bool, log *slog.Logger) (watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, &fs.PathError{Op: "inotify_init1", Path: dir, Err: err}
	}
	w := &inotifyWatcher{
		dir: dir, skip: skip, log: log, fd: fd, f: os.NewFile(uintptr(fd), "inotify"),
		sig: make(chan struct{}, 1), wds: map[int32]string{}, paths: map[string]bool{},
	}
	w.addTree(".")
	go w.read()
	return w, nil
}

// addTree watches the folder rel and every folder below it.
func (w *inotifyWatcher) addTree(rel string) {
	filepath.WalkDir(filepath.Join(w.dir, filepath.FromSlash(rel)), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		r, rerr := filepath.Rel(w.dir, p)
		if rerr != nil {
			return fs.SkipDir
		}
		r = filepath.ToSlash(r)
		if r != "." && w.skip(r) {
			return fs.SkipDir
		}
		if !w.add(r) {
			return fs.SkipAll
		}
		return nil
	})
}

// add watches the folder rel; false once the watch limit is reached.
func (w *inotifyWatcher) add(rel string) bool {
	wd, err := unix.InotifyAddWatch(w.fd, filepath.Join(w.dir, filepath.FromSlash(rel)), watchMask)
	if errors.Is(err, unix.ENOSPC) {
		w.mu.Lock()
		if !w.limit {
			w.log.Warn("sharesync: inotify watch limit reached; polling instead", "dir", w.dir)
		}
		w.limit = true
		w.mu.Unlock()
		w.notify()
		return false
	}
	if err != nil {
		// Gone, or no longer a folder: the scan sees what is there.
		return true
	}
	w.mu.Lock()
	w.wds[int32(wd)] = rel
	w.mu.Unlock()
	return true
}

// read turns inotify events into changed paths until the watcher closes.
func (w *inotifyWatcher) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.f.Read(buf)
		if err != nil {
			return
		}
		var created []string
		w.mu.Lock()
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			wd := int32(binary.NativeEndian.Uint32(buf[off:]))
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			start := off + unix.SizeofInotifyEvent
			off = start + nameLen
			if off > n {
				w.full = true
				break
			}
			name := strings.TrimRight(string(buf[start:off]), "\x00")
			if mask&unix.IN_Q_OVERFLOW != 0 {
				w.full = true
				continue
			}
			rel, ok := w.wds[wd]
			if !ok {
				continue
			}
			if mask&unix.IN_IGNORED != 0 {
				delete(w.wds, wd)
				continue
			}
			p := rel
			if name != "" {
				p = path.Join(rel, name)
			}
			w.paths[p] = true
			if mask&unix.IN_ISDIR != 0 && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
				created = append(created, p)
			}
			if mask&unix.IN_ISDIR != 0 && mask&unix.IN_MOVED_FROM != 0 {
				// The folder's watches still name its old path.
				w.full = true
			}
		}
		w.mu.Unlock()
		for _, p := range created {
			if !w.skip(p) {
				w.addTree(p)
			}
		}
		w.notify()
	}
}

func (w *inotifyWatcher) notify() {
	select {
	case w.sig <- struct{}{}:
	default:
	}
}

func (w *inotifyWatcher) ready() <-chan struct{} {
	return w.sig
}

func (w *inotifyWatcher) drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.full {
		w.full = false
		clear(w.paths)
		return []string{"."}
	}
	out := make([]string, 0, len(w.paths))
	for p := range w.paths {
		out = append(out, p)
	}
	clear(w.paths)
	return out
}

func (w *inotifyWatcher) limited() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.limit
}

func (w *inotifyWatcher) close() error {
	return w.f.Close()
}
