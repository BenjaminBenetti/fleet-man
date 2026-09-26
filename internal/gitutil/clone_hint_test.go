package gitutil

import (
	"strings"
	"testing"
)

func TestCloneFailureHint(t *testing.T) {
	const hostKey = "Host key verification failed.\nfatal: Could not read from remote repository."
	if got := CloneFailureHint("REMOTE HOST IDENTIFICATION HAS CHANGED\n"+hostKey, "git@host:r.git", AgentNotForwarded); strings.Contains(got, "retry with") || !strings.Contains(got, "will not offer") {
		t.Fatalf("changed key must not suggest approval: %q", got)
	}
	if got := CloneFailureHint(hostKey, "git@gitlab.com:team/repo.git", AgentNotForwarded); !strings.Contains(got, "host key") || !strings.Contains(got, "`ssh -T git@gitlab.com`") {
		t.Fatalf("host key failure hint = %q", got)
	}
	if got := CloneFailureHint(hostKey, "https://example.com/repo.git", AgentNotForwarded); !strings.Contains(got, "e.g. `ssh -T git@github.com`") {
		t.Fatalf("host key hint without an ssh remote = %q", got)
	}

	// The hint must name the toggle as the TUI renders it (tui.renderArmadaAgentButton).
	for _, denied := range []string{
		"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.",
		// A server that also allows other methods says so in the list.
		"git@localhost: Permission denied (publickey,password).\nfatal: Could not read from remote repository.",
	} {
		if got := CloneFailureHint(denied, "git@github.com:o/r.git", AgentNotForwarded); !strings.Contains(got, "[ agent: on ]") || !strings.Contains(got, "Settings → Fleet Armada") {
			t.Fatalf("publickey failure hint for %q = %q", denied, got)
		}
	}
	// With forwarding already on, or refused by the host, the toggle is not
	// the answer.
	const denied = "git@github.com: Permission denied (publickey)."
	if got := CloneFailureHint(denied, "", AgentForwarded); strings.Contains(got, "turn on") || !strings.Contains(got, "ssh-add -l") {
		t.Fatalf("publickey hint with a forwarded agent = %q", got)
	}
	if got := CloneFailureHint(denied, "", AgentForwardingOff); strings.Contains(got, "turn on") || !strings.Contains(got, "FLEET_SSH_AGENT_SOCK=off") {
		t.Fatalf("publickey hint with forwarding off = %q", got)
	}

	if got := CloneFailureHint("fatal: repository 'x' not found", "git@github.com:o/r.git", AgentNotForwarded); got != "" {
		t.Fatalf("unrelated failure got a hint: %q", got)
	}
}

func TestSSHTestCommand(t *testing.T) {
	for _, c := range []struct{ remote, want string }{
		{"git@github.com:owner/repo.git", "ssh -T git@github.com"},
		{"github.com:owner/repo.git", "ssh -T github.com"},
		{"ssh://git@git.example.com:2222/owner/repo.git", "ssh -T -p 2222 git@git.example.com"},
		{"git+ssh://git.example.com/owner/repo.git", "ssh -T git.example.com"},
		{"https://github.com/owner/repo.git", ""},
		{"file:///srv/repo", ""},
		{"/srv/repo.git", ""},
		{"./a:b", ""},
		{"git@evil;rm -rf ~:repo", ""},
		{"ssh://git@host;touch x/repo", ""},
	} {
		if got := sshTestCommand(c.remote); got != c.want {
			t.Errorf("sshTestCommand(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}
