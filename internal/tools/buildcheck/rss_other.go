//go:build !unix

package main

import "os"

// maxRSS is the peak resident set of an exited process; not reported here.
func maxRSS(*os.ProcessState) int64 { return -1 }
