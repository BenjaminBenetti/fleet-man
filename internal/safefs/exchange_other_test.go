//go:build unix && !linux && !darwin

package safefs

import "errors"

// exchangeAt has no atomic swap here; the attacker renames and relinks.
func exchangeAt(int, string, string) error {
	return errors.ErrUnsupported
}
