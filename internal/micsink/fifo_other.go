//go:build !linux

package micsink

import "errors"

// pipeQueued is not asked outside Linux, where instances run: the backlog cap
// is left to the full pipe's drop.
func pipeQueued(int) (int, error) { return 0, errors.ErrUnsupported }
