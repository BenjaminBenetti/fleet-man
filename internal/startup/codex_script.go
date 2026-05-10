package startup

// codexScript returns the install snippet that places the Codex CLI
// on PATH inside the container. Idempotent: short-circuits if `codex`
// is already resolvable (e.g. baked into the image).
//
// Install method: npm. The Codex CLI ships as @openai/codex on npm
// and Node.js is present in essentially every Microsoft devcontainer
// base image, so this avoids the curl-piped-bash route.
func codexScript() Script {
	return Script{
		Name: "codex",
		Body: `if command -v codex >/dev/null 2>&1; then
  echo "codex already installed: $(codex --version 2>/dev/null || echo unknown)"
  exit 0
fi
if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required to install Codex but was not found on PATH"
  exit 127
fi
echo "installing Codex via npm..."
npm install -g @openai/codex`,
	}
}
