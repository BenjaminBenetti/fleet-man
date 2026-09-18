//go:build !unix

package platform

import "os/exec"

// detachFromTerminal is a no-op where there is no unix session to leave.
func detachFromTerminal(*exec.Cmd) {}
