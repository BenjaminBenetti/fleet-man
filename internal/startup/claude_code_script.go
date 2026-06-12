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
//
// The installer hardcodes its download scratch dir to
// $HOME/.claude/downloads — and ~/.claude is this fleet's SHARED Claude
// Code mount (see dirMountSpecsFor), so instances of the same fleet
// provisioning concurrently would race on the same download file and
// clobber each other (issue #149). To keep the install entirely off the
// shared mount, the installer runs under a throwaway HOME whose .local
// is a symlink back to the real one: download scratch space and
// install-time ~/.claude state stay instance-local while the binary
// still lands in the real ~/.local/share/claude. The installer writes
// the ~/.local/bin/claude launcher symlink relative to the HOME it ran
// under, so the script re-points it at the real home before the
// throwaway dir is removed.
//
// As defense in depth the install is attempted up to three times, and
// an attempt only counts as success once the installed binary answers
// `--version` — a transient network or filesystem hiccup self-heals
// instead of leaving the instance without a `claude` binary.
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

mkdir -p "$HOME/.local"
install_home=$(mktemp -d)
trap 'rm -rf "$install_home"' EXIT
ln -s "$HOME/.local" "$install_home/.local"

attempt=1
while :; do
  echo "installing Claude Code via Anthropic installer (attempt $attempt of 3)..."
  curl -fsSL https://claude.ai/install.sh | HOME="$install_home" bash

  launcher="$HOME/.local/bin/claude"
  target=$(readlink "$launcher" 2>/dev/null || true)
  case "$target" in
    "$install_home"/*) ln -sf "$HOME${target#"$install_home"}" "$launcher" ;;
  esac

  if "$launcher" --version >/dev/null 2>&1; then
    echo "claude installed: $("$launcher" --version)"
    exit 0
  fi
  if [ "$attempt" -ge 3 ]; then
    echo "Claude Code install failed after 3 attempts"
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 2
done`,
	}
}
