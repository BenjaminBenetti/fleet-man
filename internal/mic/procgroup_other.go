//go:build !unix

package mic

import "os/exec"

// killWholeGroup is a no-op where there are no unix process groups; cancelling
// kills the direct child only.
func killWholeGroup(*exec.Cmd) {}
