package startup

// auggieScript returns the install snippet that places the Auggie CLI
// (Augment Code's coding agent) on PATH inside the container.
// Idempotent: short-circuits if `auggie` is already resolvable (e.g.
// baked into the image).
//
// Install method: npm. The Auggie CLI ships as @augmentcode/auggie on
// npm — the official install path per docs.augmentcode.com — and
// Node.js is present in essentially every Microsoft devcontainer base
// image, so this mirrors the Codex install route.
func auggieScript() Script {
	return Script{
		Name: "auggie",
		Body: `if command -v auggie >/dev/null 2>&1; then
  echo "auggie already installed: $(auggie --version 2>/dev/null || echo unknown)"
  exit 0
fi
if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required to install Auggie but was not found on PATH"
  exit 127
fi
echo "installing Auggie via npm..."
npm install -g @augmentcode/auggie`,
	}
}
