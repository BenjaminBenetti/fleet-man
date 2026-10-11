package micsink

import "golang.org/x/sys/unix"

// pipeQueued is how many bytes sit unread in the pipe fd (FIONREAD).
func pipeQueued(fd int) (int, error) { return unix.IoctlGetInt(fd, unix.TIOCINQ) }
