//go:build darwin

package safefs

import (
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// exchangeMu runs the attackers' swaps one at a time: two RENAME_SWAPs at
// once can deadlock in the kernel (macOS runners left both attackers in
// renameatx_np, the process in uninterruptible wait). The attackers still
// race the walker, which is what the test is for.
var exchangeMu sync.Mutex

// exchangeLock is a file lock that does the same across processes: go test
// -fuzz runs a worker process per CPU, each with its own exchangeMu (a macOS
// fuzz run wedged silently with its workers swapping at once). It is -1 when
// the lock file cannot be opened.
var exchangeLock = sync.OnceValue(func() int {
	fd, err := unix.Open(filepath.Join(os.TempDir(), "fleet-safefs-exchange.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1
	}
	return fd
})

// exchangeAt atomically swaps the entries a and b of the folder dirFD, so the
// attacker flips a path between a folder and a link with no gap between.
func exchangeAt(dirFD int, a, b string) error {
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	if fd := exchangeLock(); fd >= 0 && unix.Flock(fd, unix.LOCK_EX) == nil {
		defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	}
	return unix.RenameatxNp(dirFD, a, dirFD, b, unix.RENAME_SWAP)
}
