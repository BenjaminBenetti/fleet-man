package devcontainer

import "testing"

func TestRemoteUserFromMetadataJSON(t *testing.T) {
	// Trimmed from a real devcontainer.metadata label: remoteUser is declared
	// on one feature entry, the user's own config (last) declares none.
	realLabel := `[ {"id":"ghcr.io/devcontainers/features/common-utils:2"},` +
		` {"id":"ghcr.io/devcontainers/features/node:1"},` +
		` {"customizations":{"vscode":{"extensions":["ms-dotnettools.csharp"]}},"remoteUser":"vscode"},` +
		` {"id":"ghcr.io/devcontainers/features/github-cli:1"},` +
		` {"postCreateCommand":"./.devcontainer/postCreate.sh","customizations":{"vscode":{"extensions":["anthropic.claude-code"]}}} ]`

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"real label", realLabel, "vscode"},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"malformed", "[ not json", ""},
		{"no users", `[{"id":"a"},{"id":"b"}]`, ""},
		{"single remoteUser", `[{"remoteUser":"node"}]`, "node"},
		// Later array entries override earlier ones (devcontainer.json is last).
		{"last remoteUser wins", `[{"remoteUser":"vscode"},{"remoteUser":"root"}]`, "root"},
		// containerUser is the fallback when no entry names a remoteUser.
		{"containerUser fallback", `[{"containerUser":"app"}]`, "app"},
		{"remoteUser beats containerUser", `[{"containerUser":"app","remoteUser":"vscode"}]`, "vscode"},
		{"last containerUser wins", `[{"containerUser":"app"},{"containerUser":"dev"}]`, "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remoteUserFromMetadataJSON(tt.raw); got != tt.want {
				t.Errorf("remoteUserFromMetadataJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}
