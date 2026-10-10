package backend

import (
	"context"
	"io"
)

// Hooks are server-owned steps that an UpContext implementation runs at fixed
// points of its sequence. The server imports the backends, so a backend can't
// call into the server directly; the job that calls UpContext injects them
// through ctx (WithHooks) instead. Each hook is a closure the server has
// already bound to the one instance being provisioned. A nil hook is skipped.
type Hooks struct {
	// EnsureAgent brings up the daemon's session into the new container (the
	// sockets and relays it serves). It runs once the backend has verified the
	// container and before any lifecycle hook (devcontainer onCreate/postCreate
	// commands) runs, so those hooks already see the sockets.
	EnsureAgent func(ctx context.Context) error

	// SharedStateReady runs after EnsureAgent and before lifecycle hooks. It
	// blocks until the fleet's shared state (agent logins, settings) is in place
	// inside the instance, so the hooks find it. A failure is a warning on the
	// instance, never a failed provision.
	SharedStateReady func(ctx context.Context) error
}

type hooksKey struct{}

// WithHooks returns a copy of ctx that carries h for an UpContext call.
func WithHooks(ctx context.Context, h Hooks) context.Context {
	return context.WithValue(ctx, hooksKey{}, h)
}

// HooksFrom returns the Hooks that ctx carries, or the zero Hooks (every hook
// nil, so every hook is skipped) when the caller injected none.
func HooksFrom(ctx context.Context) Hooks {
	h, _ := ctx.Value(hooksKey{}).(Hooks)
	return h
}

type jobLogKey struct{}

// WithJobLog returns a copy of ctx whose provisioning-log sink is w. An
// UpContext implementation writes the output of each provisioning step there,
// on top of what it logs to fleet.log.
func WithJobLog(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, jobLogKey{}, w)
}

// JobLogFrom returns ctx's provisioning-log sink, or io.Discard when the caller
// attached none, so implementations can write to it unconditionally.
func JobLogFrom(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(jobLogKey{}).(io.Writer); ok && w != nil {
		return w
	}
	return io.Discard
}
