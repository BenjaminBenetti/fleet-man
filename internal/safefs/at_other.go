//go:build !unix

package safefs

import (
	"errors"
	"io/fs"
	"time"
)

// Fleet ships for Linux, macOS and the BSDs; elsewhere the no-follow *at
// calls are not wired and these operations are refused.
const openGuardFlags = 0

func chmodAt(*dir, string, fs.FileMode) error { return errors.ErrUnsupported }

func chtimesAt(*dir, string, time.Time, time.Time) error { return errors.ErrUnsupported }

func renameAt(*dir, string, *dir, string) error { return errors.ErrUnsupported }
