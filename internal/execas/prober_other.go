//go:build !linux

package execas

import "context"

// Prober probes nothing off linux, where no command runs.
type Prober struct{}

// NewProber is a Prober that probes nothing.
func NewProber(string) *Prober { return &Prober{} }

// ProbeEnv returns nothing off linux.
func (*Prober) ProbeEnv(context.Context, ProbeTarget) map[string]string { return nil }
