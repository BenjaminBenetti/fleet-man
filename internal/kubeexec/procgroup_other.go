//go:build !nok8s && !unix

package kubeexec

import "os/exec"

// ownProcessGroup leaves cmd as it is: cancel kills the direct child.
func ownProcessGroup(*exec.Cmd) {}
