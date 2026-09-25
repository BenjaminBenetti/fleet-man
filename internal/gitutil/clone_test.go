package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCloneSSHTarget(t *testing.T) {
	for _, tt := range []struct{ remote, target string }{
		{"git@host:repo.git", "ssh://git@host"},
		{"host:repo.git", "ssh://host"},
		{"ssh://git@host:2222/repo.git", "ssh://git@host:2222"},
		{"git+ssh://git@host/repo.git", "ssh://git@host"},
		{"git@[::1]:repo.git", "ssh://git@[::1]"},
		{"ssh://git@[::1]:2222/repo.git", "ssh://git@[::1]:2222"},
		{"https://host/repo.git", ""},
		{"file:///repo.git", ""},
		{"./path:repo", ""},
		{"-oProxyCommand=x:repo", ""},
		{"ssh://-oProxyCommand=x/repo", ""},
		{"ssh://git:password@host/repo", ""},
	} {
		t.Run(tt.remote, func(t *testing.T) {
			target, ok := cloneSSHTarget(tt.remote)
			if ok != (tt.target != "") || (ok && target.String() != tt.target) {
				t.Fatalf("%q: target=%q ok=%v", tt.remote, target.String(), ok)
			}
		})
	}
}

func TestClonePreservesExplicitSSHTransport(t *testing.T) {
	for _, source := range []string{"GIT_SSH_COMMAND", "GIT_SSH", "core.sshCommand"} {
		t.Run(source, func(t *testing.T) {
			for _, name := range []string{"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT"} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
			script := filepath.Join(t.TempDir(), "custom-ssh")
			marker := filepath.Join(t.TempDir(), "called")
			t.Setenv("FLEET_TEST_SSH_MARKER", marker)
			if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$FLEET_TEST_SSH_MARKER\"\necho 'Host key verification failed.' >&2\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if source == "core.sshCommand" {
				if err := exec.Command("git", "config", "--global", source, script).Run(); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv(source, script)
			}
			dest := filepath.Join(t.TempDir(), "clone")
			out, err := Clone(context.Background(), "git@unused:repo.git", []string{"clone", "--", "git@unused:repo.git", dest}, nil)
			if err == nil {
				t.Fatal("custom transport should have failed")
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("custom SSH transport not used: %v %s", err, out)
			}
		})
	}
}
