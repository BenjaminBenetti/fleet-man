//go:build !(linux || darwin || freebsd)

package sharesync

import "errors"

// diskSpace is unknown here: no free-room check runs, and a full disk shows
// only as the write failing.
func diskSpace(string) (Space, error) {
	return Space{}, errors.ErrUnsupported
}
