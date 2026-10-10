//go:build unix

package main

import (
	"os"
	"runtime"
	"syscall"
)

// maxRSS is the peak resident set of an exited process in bytes, or -1.
func maxRSS(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return -1
	}
	// ru_maxrss is in bytes on darwin, KiB on linux and the BSDs.
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) << 10
}
