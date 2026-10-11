//go:build !linux

package safefs

import "errors"

// FastPath reports whether walks use openat2(2), which only Linux has.
func FastPath() bool {
	return false
}

// openFast is the Linux openat2 walk; elsewhere every walk is element by
// element.
func (r *Root) openFast(string) (*dir, error) {
	return nil, errors.ErrUnsupported
}
