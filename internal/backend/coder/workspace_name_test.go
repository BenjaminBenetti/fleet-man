package coder

import "testing"

// TestWorkspaceNameFor pins the "<prefix>-<instance>" composition and its
// sanitization (lowercase, coder's 32-char cap).
func TestWorkspaceNameFor(t *testing.T) {
	if got := WorkspaceNameFor("MyProj", "agent-1"); got != "myproj-agent-1" {
		t.Fatalf("WorkspaceNameFor = %q, want %q", got, "myproj-agent-1")
	}
	if got := WorkspaceNameFor("a-very-long-workspace-prefix", "instance-name"); len(got) > coderMaxNameLen {
		t.Fatalf("WorkspaceNameFor did not cap at %d chars: %q (%d)", coderMaxNameLen, got, len(got))
	}
}

// TestResolveWorkspaceNamePrecedence guards the load-bearing name resolution
// for path-based methods (rawExec/EditorURI): registered/created names win,
// then the explicitly configured name, then the historical path derivation —
// the fallback that keeps pre-#221 workspaces addressable.
func TestResolveWorkspaceNamePrecedence(t *testing.T) {
	const wsDir = "/home/u/.fleet/workspaces/alpha/agent-1/alpha"

	// No configuration at all: historical "{fleet}-{instance}" derivation.
	b := New()
	if got := b.resolveWorkspaceName(wsDir); got != "alpha-agent-1" {
		t.Fatalf("path-derived fallback = %q, want %q", got, "alpha-agent-1")
	}

	// Configured explicit name beats path derivation.
	b = New(WithWorkspaceName("custom-agent-1"))
	if got := b.resolveWorkspaceName(wsDir); got != "custom-agent-1" {
		t.Fatalf("configured name = %q, want %q", got, "custom-agent-1")
	}

	// A registered name (recorded container ID, possibly "ws.agent") beats both.
	b.RegisterName(wsDir, "custom-agent-1.dev")
	if got := b.resolveWorkspaceName(wsDir); got != "custom-agent-1.dev" {
		t.Fatalf("registered name = %q, want %q", got, "custom-agent-1.dev")
	}
	// ...but only for its own workspace dir.
	if got := b.resolveWorkspaceName("/home/u/.fleet/workspaces/alpha/agent-2/alpha"); got != "custom-agent-1" {
		t.Fatalf("other dir should not see the registration: %q", got)
	}
}

// TestEditorURIUsesResolvedName verifies EditorURI targets the resolved
// workspace (agent suffix stripped — editor URIs address the workspace).
func TestEditorURIUsesResolvedName(t *testing.T) {
	const wsDir = "/home/u/.fleet/workspaces/alpha/agent-1/alpha"
	b := New()
	b.RegisterName(wsDir, "custom-agent-1.dev")
	uri, ok := b.EditorURI(wsDir, "alpha")
	if !ok || uri != "coder-vscode://custom-agent-1" {
		t.Fatalf("EditorURI = %q, %v; want coder-vscode://custom-agent-1, true", uri, ok)
	}
}

// TestRawExecTargetsRegisteredName verifies the exec path addresses the
// registered workspace: the built `coder ssh` argv must reference the
// recorded name, not a path-derived one. A "ws.agent" registration also
// skips the devcontainer-agent probe in resolveSSHTarget, keeping this test
// exec-free.
func TestRawExecTargetsRegisteredName(t *testing.T) {
	const wsDir = "/home/u/.fleet/workspaces/alpha/agent-1/alpha"
	b := New()
	b.RegisterName(wsDir, "custom-agent-1.dev")
	cmd := b.rawExec(wsDir, []string{"true"})
	found := false
	for _, arg := range cmd.Args {
		if arg == "custom-agent-1.dev" {
			found = true
		}
		if arg == "alpha-agent-1" {
			t.Fatalf("rawExec used the path-derived name: %v", cmd.Args)
		}
	}
	if !found {
		t.Fatalf("rawExec argv missing the registered target: %v", cmd.Args)
	}
}
