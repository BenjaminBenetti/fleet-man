package backend

import "context"

// TmuxEnsurer is an optional capability for a backend that ships its own tmux
// for images that have none: before an interactive attach runs the tmux
// command, the daemon calls EnsureTmux so the binary is present (installed
// once, content-addressed). Backends whose image is expected to carry tmux,
// or that install it another way, do not implement it. A failure is not fatal
// to the attach — the attach's own tmux command reports a missing binary.
type TmuxEnsurer interface {
	EnsureTmux(ctx context.Context) error
}
