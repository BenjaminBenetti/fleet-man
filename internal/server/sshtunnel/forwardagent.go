package sshtunnel

import "context"

// ForwardAgentConfigured reports whether the user's ssh config forwards their
// default agent to the host behind an ssh:// URL: ForwardAgent, as `ssh -G`
// resolves it (so Host aliases and Match blocks apply), is yes or
// $SSH_AUTH_SOCK. A newly registered ssh:// Armada remote starts with agent
// forwarding on only then. ForwardAgent naming a socket path or another
// variable forwards a different (often restricted) agent, and fleet can only
// forward SSH_AUTH_SOCK's — so it reads as "no", as does any failure.
func ForwardAgentConfigured(ctx context.Context, rawURL string) bool {
	t, err := ParseURL(rawURL)
	if err != nil {
		return false
	}
	c, err := resolveSSHConfig(ctx, t)
	if err != nil {
		return false
	}
	return c.forwardAgent
}
