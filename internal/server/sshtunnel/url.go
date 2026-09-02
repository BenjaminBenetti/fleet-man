// Package sshtunnel maintains, on the LOCAL fleet daemon, one SSH port-forward
// per ssh:// fleet-armada remote so a `fleet` client (the TUI, a CLI command, a
// `fleet shell` child) can reach a remote daemon's token-gated gRPC server
// without a fleet gateway: the remote daemon listens on a loopback port
// (Settings → Fleet MCP → Remote Fleet via SSH; see internal/server/sshlisten.go)
// and this package tunnels to it with the user's own `ssh` binary, so
// ~/.ssh/config aliases, keys, agents, and ProxyJump all apply unchanged.
//
// The daemon — not the TUI — owns the tunnels because the armada registry
// already lives there, a tunnel then outlives any one TUI (tmux panes spawned
// by `fleet shell` keep working across a TUI restart), and every client on the
// machine shares one ssh process per remote instead of spawning its own. The
// entry point is Manager.Resolve, served by the local-only ResolveArmadaRemote
// RPC: it discovers the remote listener's port + bearer token over SSH (a small
// POSIX sh script, no remote `fleet` command needed), brings the forward up,
// verifies it end-to-end with a Hello RPC, and returns the loopback address the
// client should dial. There is no reconnect loop: a dead or stale forward is
// rebuilt on the next Resolve, which every client dial triggers.
package sshtunnel

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Target is a parsed ssh:// URL: where to ssh. Port is "" for the ssh default
// (the user's ~/.ssh/config still applies, so Host may be an alias).
type Target struct {
	User string
	Host string
	Port string
}

// IsSSHURL reports whether raw is an ssh:// URL (scheme compared
// case-insensitively). Mirrors fleetclient.IsSSHURL, which the import boundary
// keeps separate.
func IsSSHURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "ssh://")
}

// ParseURL parses ssh://[user@]host[:port]. It rejects anything that would let
// the host or user be read by ssh as an option (a leading "-"), a path/query
// (nothing to do with them), and an empty host.
func ParseURL(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if !IsSSHURL(raw) {
		return Target{}, fmt.Errorf("not an ssh:// URL: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("parse ssh url: %w", err)
	}
	t := Target{Host: strings.ToLower(u.Hostname()), Port: u.Port()}
	if u.User != nil {
		t.User = u.User.Username()
		if _, hasPw := u.User.Password(); hasPw {
			return Target{}, fmt.Errorf("ssh url must not carry a password: use keys or an agent")
		}
	}
	if t.Host == "" {
		return Target{}, fmt.Errorf("ssh url has no host: %q", raw)
	}
	if strings.HasPrefix(t.Host, "-") || strings.HasPrefix(t.User, "-") {
		return Target{}, fmt.Errorf("ssh url host/user must not start with '-': %q", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Target{}, fmt.Errorf("ssh url must be ssh://[user@]host[:port] with no path: %q", raw)
	}
	return t, nil
}

// String renders the canonical URL (ssh://[user@]host[:port]) — the key the
// Manager dedupes tunnels on, so two spellings of one remote share a tunnel.
func (t Target) String() string {
	var b strings.Builder
	b.WriteString("ssh://")
	if t.User != "" {
		b.WriteString(t.User)
		b.WriteByte('@')
	}
	if strings.Contains(t.Host, ":") {
		b.WriteString("[" + t.Host + "]") // IPv6 literal
	} else {
		b.WriteString(t.Host)
	}
	if t.Port != "" {
		b.WriteByte(':')
		b.WriteString(t.Port)
	}
	return b.String()
}

// destination is the ssh command's destination operand ([user@]host).
func (t Target) destination() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// sshBaseArgs are the options every ssh invocation starts with: batch mode so
// a daemon with no TTY can never hang on a prompt (an unknown host key or a
// passphrase-locked key fails fast with a clear stderr instead), a bounded
// connect, and keepalives so a dead peer is noticed. A package var so tests can
// prepend "-F <config>" — OpenSSH resolves ~/.ssh from the passwd entry, not
// $HOME, so a test cannot redirect it any other way.
var sshBaseArgs = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=15",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
}

// sshArgs builds the argv (after "ssh") for one invocation: the base options,
// the port, the caller's extra flags, then the destination.
func (t Target) sshArgs(extra ...string) []string {
	args := append([]string(nil), sshBaseArgs...)
	if t.Port != "" {
		args = append(args, "-p", t.Port)
	}
	args = append(args, extra...)
	return append(args, t.destination())
}

// loopback renders 127.0.0.1:<port>.
func loopback(port int) string {
	return net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
}
