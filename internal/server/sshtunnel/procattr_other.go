//go:build !linux

package sshtunnel

import "os/exec"

// setSysProcAttr is a no-op off Linux (no parent-death signal there); an
// orphaned forward is bounded by Manager.Close on orderly shutdown and by ssh's
// own keepalives.
func setSysProcAttr(*exec.Cmd) {}
