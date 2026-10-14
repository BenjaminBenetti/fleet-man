//go:build darwin

package safefs

import (
	"sync"

	"golang.org/x/sys/unix"
)

// exchangeMu runs the attackers' swaps one at a time: two RENAME_SWAPs at
// once in one tree can deadlock in the kernel (macOS runners left both
// attackers in renameatx_np, the process in uninterruptible wait). The
// attackers still race the walker, which is what the test is for.
var exchangeMu sync.Mutex

// exchangeAt atomically swaps the entries a and b of the folder dirFD, so the
// attacker flips a path between a folder and a link with no gap between.
func exchangeAt(dirFD int, a, b string) error {
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	return unix.RenameatxNp(dirFD, a, dirFD, b, unix.RENAME_SWAP)
}
