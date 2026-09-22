package gitutil

import (
	"strings"
	"testing"
)

func TestCloneFailureHint(t *testing.T) {
	if got := CloneFailureHint("Host key verification failed.\nfatal: Could not read from remote repository."); !strings.Contains(got, "host key") {
		t.Fatalf("host key failure hint = %q", got)
	}
	if got := CloneFailureHint("git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository."); !strings.Contains(got, "Forward SSH agent") {
		t.Fatalf("publickey failure hint = %q", got)
	}
	if got := CloneFailureHint("fatal: repository 'x' not found"); got != "" {
		t.Fatalf("unrelated failure got a hint: %q", got)
	}
}
