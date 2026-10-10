//go:build !linux

package execas

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// AbortGrace is how long Abort waits before it kills what is left.
const AbortGrace = 5 * time.Second

// errNotLinux is what starting anything says off linux: commands run in pods.
var errNotLinux = errors.New("execas runs commands only inside a linux pod")

// CanSwitchUser is always false off linux.
func CanSwitchUser() bool { return false }

// CurrentCreds are this process's own uid, gid and supplementary groups.
func CurrentCreds() Creds {
	creds := Creds{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	groups, _ := os.Getgroups()
	for _, g := range groups {
		creds.Groups = append(creds.Groups, uint32(g))
	}
	return creds
}

// Stdio is a command's standard streams when it has no TTY.
type Stdio struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	WaitDelay time.Duration
}

// Process is never started off linux.
type Process struct{}

// Start fails off linux.
func Start(*Plan, Stdio) (*Process, error) { return nil, failed(errNotLinux) }

// Exec fails off linux.
func Exec(*Plan) error { return failed(errNotLinux) }

func isTerminal(*os.File) bool { return false }

func (*Process) Pid() int                    { return 0 }
func (*Process) PTY() *os.File               { return nil }
func (*Process) Resize(uint16, uint16) error { return errNotLinux }
func (*Process) Wait() (ExitStatus, error)   { return ExitStatus{Code: -1}, errNotLinux }
func (*Process) Signal(syscall.Signal) error { return errNotLinux }
func (*Process) Abort()                      {}
func (*Process) Kill()                       {}
