package sshtunnel

import "context"

// ForwardAgentConfigured reports whether the user's ssh config forwards an
// agent to the host behind an ssh:// URL (ForwardAgent, as `ssh -G` resolves
// it — so Host aliases and Match blocks apply). A newly registered ssh://
// Armada remote starts with agent forwarding set the way the user already set
// it for a plain `ssh` to that host. Any failure reads as "no".
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
