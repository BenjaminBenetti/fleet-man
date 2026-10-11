//go:build linux

package safefs

import "golang.org/x/sys/unix"

// exchangeAt atomically swaps the entries a and b of the folder dirFD, so the
// attacker flips a path between a folder and a link with no gap between.
func exchangeAt(dirFD int, a, b string) error {
	return unix.Renameat2(dirFD, a, dirFD, b, unix.RENAME_EXCHANGE)
}
