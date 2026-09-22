package gitutil

import "strings"

// CloneFailureHint returns a one-line explanation for the two ssh failures a
// clone run by a detached fleet daemon typically hits, or "" for anything
// else. The daemon has no terminal, so ssh can neither ask about an unknown
// host key nor prompt for a passphrase: both surface as terse git errors that
// do not say what to do.
func CloneFailureHint(output string) string {
	switch {
	case strings.Contains(output, "Host key verification failed"):
		return "hint: this host does not know the git server's SSH host key yet, and the fleet daemon cannot ask — connect to it once from this host (e.g. `ssh -T git@github.com`) to trust it"
	case strings.Contains(output, "Permission denied (publickey)"):
		return "hint: no SSH key on this host is accepted by the git server — on a remote fleet, turn on \"Forward SSH agent\" for it in the TUI's Fleet Armada settings to use your own keys"
	default:
		return ""
	}
}
