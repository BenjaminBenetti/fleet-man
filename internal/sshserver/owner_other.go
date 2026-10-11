//go:build !linux

package sshserver

// socketOwnedBy needs Linux's /proc; fleet instances are Linux, so off
// Linux no forward is VS Code's.
func socketOwnedBy(string, string, []string) bool { return false }

// runsFrom needs /proc too: off Linux no VS Code server runs.
func runsFrom(string) bool { return false }
