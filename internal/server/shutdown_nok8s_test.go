//go:build nok8s

package server

import "testing"

// stubServeShutdownPool: a -tags nok8s serve loop has no pool to end.
func stubServeShutdownPool(*testing.T) {}
