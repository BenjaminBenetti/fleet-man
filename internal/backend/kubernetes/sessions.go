//go:build !nok8s

package kubernetes

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// sessions.go selects the tmux an instance's sessions use: the image's own
// when it has one, else the static tmux fleet bundles (installed into the
// pod, then on every spawn's PATH via the agent's bundle wiring). Nothing in
// a pod ever runs apt. An attach calls EnsureTmux before it runs `tmux
// new-session -A`, so a tmux-less image's first attach finds the bundled
// binary rather than failing.

// ensureTmuxTimeout bounds the probe and the one-time bundle install.
const ensureTmuxTimeout = 3 * time.Minute

// EnsureTmux makes `tmux` resolvable in the instance's pod: it returns at
// once when the image already provides tmux, and otherwise installs the
// bundled static tmux (idempotent — a no-op once installed). Best effort for
// an attach; a failure leaves the attach to report "tmux: not found".
func (b *Backend) EnsureTmux(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ensureTmuxTimeout)
	defer cancel()
	s, err := b.agent(ctx, false)
	if err != nil {
		return err
	}
	// `command -v tmux` finds the image's tmux, or the bundle once it is
	// installed (the agent puts the bundle's bin last on PATH).
	res, err := s.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", "command -v tmux"}}}, nil)
	if err == nil && res.Code == 0 {
		return nil
	}
	_, err = s.EnsureTmux(ctx)
	return err
}
