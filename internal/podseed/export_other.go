//go:build !linux

package podseed

import "errors"

// errLinuxOnly: the in-pod commands run inside linux pods.
var errLinuxOnly = errors.New("only inside a linux pod")

// NestedMounts fails off linux.
func NestedMounts(string) ([]string, error) { return nil, errLinuxOnly }

// DiskUsage fails off linux.
func DiskUsage(string) (DiskSpace, error) { return DiskSpace{}, errLinuxOnly }
