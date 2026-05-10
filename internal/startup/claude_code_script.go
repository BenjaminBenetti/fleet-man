package startup

// claudeCodeScript returns the install snippet that places the Claude
// Code CLI on PATH inside the container. The script is idempotent:
// if `claude` is already resolvable (e.g. baked into the image) it
// short-circuits without re-installing.
//
// Install method: Anthropic's native installer at claude.ai/install.sh,
// which drops a pre-built binary into ~/.local/bin/claude. The npm
// distribution (@anthropic-ai/claude-code) is deprecated upstream and
// is no longer the recommended install path.
func claudeCodeScript() Script {
	return Script{
		Name: "claude-code",
		Body: `if command -v claude >/dev/null 2>&1; then
  echo "claude already installed: $(claude --version 2>/dev/null || echo unknown)"
  exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to install Claude Code but was not found on PATH"
  exit 127
fi
echo "installing Claude Code via Anthropic installer..."
curl -fsSL https://claude.ai/install.sh | bash`,
	}
}
