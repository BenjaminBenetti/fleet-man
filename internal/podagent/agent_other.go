//go:build !linux

package podagent

import (
	"context"
	"errors"
	"io"
)

// errNotLinux is what the agent's entry points say off linux: it runs in pods.
var errNotLinux = errors.New("the pod agent runs only inside a linux pod")

// Serve fails off linux.
func Serve(context.Context, io.ReadWriteCloser, Options) error { return errNotLinux }

// ExecStdio fails off linux.
func ExecStdio(context.Context, io.Reader, io.Writer, Options, string) error { return errNotLinux }

// RunShepherd fails off linux.
func RunShepherd() error { return errNotLinux }

// RunSpoke fails off linux.
func RunSpoke() error { return errNotLinux }

// TakeStdio fails off linux.
func TakeStdio() (io.ReadWriteCloser, error) { return nil, errNotLinux }
