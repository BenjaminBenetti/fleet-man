package gitutil

import (
	"net/url"
	"regexp"
	"strings"
)

// AgentForwarding is what the SSH agent of the process that ran a clone could
// offer the git server, as far as agent forwarding goes.
type AgentForwarding int

const (
	// AgentNotForwarded: no client was forwarding its agent — the host's own
	// keys, if any, were all the clone could offer.
	AgentNotForwarded AgentForwarding = iota
	// AgentForwarded: a client was attached to forward its agent to this host.
	// Whether that agent served the clone is not known: it may hold no key,
	// be unreachable on the client's machine, or not be answering at all.
	AgentForwarded
	// AgentForwardingOff: this host refuses agent forwarding
	// (FLEET_SSH_AGENT_SOCK=off), so no client's keys can reach it. The agent
	// the daemon was started with still serves its clones.
	AgentForwardingOff
)

// CloneFailureHint returns a one-line explanation for the two ssh failures a
// clone run by a detached fleet daemon typically hits, or "" for anything
// else. The daemon has no terminal, so ssh can neither ask about an unknown
// host key nor prompt for a passphrase: both surface as terse git errors that
// do not say what to do. remote is the URL that was cloned (it names the host
// to trust); agent says what forwarding could have offered, so the hint never
// asks for a toggle that is already on.
func CloneFailureHint(output, remote string, agent AgentForwarding) string {
	switch {
	case strings.Contains(output, "Host key verification failed"):
		try := "e.g. `ssh -T git@github.com`"
		if cmd := sshTestCommand(remote); cmd != "" {
			try = "`" + cmd + "`"
		}
		return "hint: this host does not know the git server's SSH host key yet, and the fleet daemon cannot ask — connect to it once from this host (" + try + ") to trust it"
	// No closing paren: servers that also offer other methods answer
	// "(publickey,password)" and the like.
	case strings.Contains(output, "Permission denied (publickey"):
		switch agent {
		case AgentForwarded:
			return "hint: the git server accepted none of the keys offered, including any from the agent a connected client forwards — run `ssh-add -l` on your machine to check that it holds a key with access to this repository"
		case AgentForwardingOff:
			return "hint: no SSH key this host offered is accepted by the git server, and this host has SSH agent forwarding turned off (FLEET_SSH_AGENT_SOCK=off), so it cannot use keys forwarded from another machine"
		default:
			// Names the control exactly as the TUI draws it: the toggle on
			// the remote's row in Settings → Fleet Armada.
			return "hint: no SSH key on this host is accepted by the git server — on a remote fleet, turn on [ agent: on ] on its row in Settings → Fleet Armada (right arrow, enter) to use your own keys, and keep the TUI connected"
		}
	default:
		return ""
	}
}

// sshTargetPart is what sshTestCommand will put in a suggested command: a
// remote's user, host and port come from the repository URL, so anything
// beyond a plain name is left out rather than quoted.
var sshTargetPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// sshTestCommand is the `ssh -T` that trusts the host of an ssh remote — scp
// syntax ([user@]host:path) or an ssh:// URL — or "" when remote is not one.
func sshTestCommand(remote string) string {
	var user, host, port string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		switch u.Scheme {
		case "ssh", "git+ssh", "ssh+git":
		default:
			return ""
		}
		user, host, port = u.User.Username(), u.Hostname(), u.Port()
	} else {
		// git reads scp syntax only when a colon comes before any slash.
		colon := strings.Index(remote, ":")
		if colon <= 0 || strings.Contains(remote[:colon], "/") {
			return ""
		}
		host = remote[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			user, host = host[:at], host[at+1:]
		}
	}
	if !sshTargetPart.MatchString(host) || (user != "" && !sshTargetPart.MatchString(user)) || (port != "" && !sshTargetPart.MatchString(port)) {
		return ""
	}
	cmd := "ssh -T "
	if port != "" {
		cmd += "-p " + port + " "
	}
	if user != "" {
		cmd += user + "@"
	}
	return cmd + host
}
