package startup

// codexScript installs the complete standalone package, including the Code Mode
// host and bundled resources, using OpenAI's official installer. A bare release
// binary can answer --version while failing to start Code Mode.
//
// CODEX_HOME is overridden only for the installer: packages and install locks
// stay container-local instead of racing on the fleet's shared ~/.codex mount.
// Runtime config and authentication still use the normal Codex home.
// Existing image-provided installs are preserved; incomplete Fleet installs in
// ~/.local/bin are repaired. Installation needs no Node.js or npm.
func codexScript() Script {
	return Script{
		Name: "codex",
		Body: `launcher="$HOME/.local/bin/codex"
install_home="$HOME/.local/share/fleet/codex"
package="$install_home/packages/standalone/current"

install_complete() {
  [ "$launcher" -ef "$package/bin/codex" ] &&
    [ -f "$package/codex-package.json" ] &&
    [ -x "$package/bin/codex-code-mode-host" ] &&
    [ -x "$package/codex-path/rg" ] &&
    [ -x "$package/codex-resources/bwrap" ] &&
    "$launcher" --version >/dev/null 2>&1
}

existing=$(command -v codex 2>/dev/null || true)
if { [ -n "$existing" ] && [ "$existing" != "$launcher" ]; } || install_complete; then
  echo "codex already installed: $("${existing:-$launcher}" --version 2>/dev/null || echo unknown)"
  exit 0
fi
for tool in curl tar; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required to install Codex but was not found on PATH"
    exit 127
  fi
done

scratch=$(mktemp -d) || exit 1
trap 'rm -rf "$scratch"' EXIT

attempt=1
while :; do
  echo "installing Codex via OpenAI installer (attempt $attempt of 3)..."
  if curl -fsSL https://chatgpt.com/codex/install.sh -o "$scratch/install.sh" &&
    CODEX_HOME="$install_home" CODEX_INSTALL_DIR="$HOME/.local/bin" CODEX_NON_INTERACTIVE=1 sh "$scratch/install.sh" &&
    install_complete; then
    echo "codex installed: $("$launcher" --version)"
    grep -qs '\.local/bin' "$HOME/.profile" ||
      printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >>"$HOME/.profile"
    exit 0
  fi
  if [ "$attempt" -ge 3 ]; then
    echo "Codex install failed after 3 attempts"
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 2
done`,
	}
}
