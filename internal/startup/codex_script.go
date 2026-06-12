package startup

// codexScript returns the install snippet that places the Codex CLI
// on PATH inside the container. Idempotent: short-circuits if `codex`
// is already resolvable (e.g. baked into the image).
//
// Install method: the static musl binary from the latest GitHub
// release, unpacked into ~/.local/bin/codex — the same directory the
// Claude Code installer targets, so codex is visible in exactly the
// shells claude is. Needs only curl + tar.
//
// Two distribution channels were rejected (issue #145):
//   - npm (@openai/codex): silently broke on any image without
//     Node.js — npm-less images exited 127, root-owned system Node
//     hit EACCES on `npm -g`, and nvm-based installs landed the
//     binary somewhere only nvm-initialized shells could see.
//   - OpenAI's official installer (chatgpt.com/codex/install.sh):
//     its SHA256SUMS lookup uses an awk interval regex ({64}) that
//     mawk — the default awk on Debian/Ubuntu base images — does not
//     support, so it aborts with "Could not find SHA-256 digest" on
//     exactly the npm-less images this script needs to cover. It also
//     unpacks its payload under ~/.codex, this fleet's SHARED Codex
//     mount (see dirMountSpecsFor), which the direct download avoids
//     entirely — no shared-mount writes means no #149-class install
//     races between concurrently provisioning instances.
//
// The release tarball contains a single statically-linked
// (musl-target) binary, so it runs on any Linux regardless of libc.
// ~/.local/bin is appended to ~/.profile when no .local/bin entry is
// present so login shells resolve codex even on images whose stock
// .profile lacks the conventional Debian snippet.
//
// As defense in depth the install is attempted up to three times, and
// an attempt only counts as success once the installed binary answers
// `--version`, matching claudeCodeScript.
func codexScript() Script {
	return Script{
		Name: "codex",
		Body: `if command -v codex >/dev/null 2>&1; then
  echo "codex already installed: $(codex --version 2>/dev/null || echo unknown)"
  exit 0
fi
for tool in curl tar; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required to install Codex but was not found on PATH"
    exit 127
  fi
done

case "$(uname -m)" in
  x86_64 | amd64)  target="x86_64-unknown-linux-musl" ;;
  aarch64 | arm64) target="aarch64-unknown-linux-musl" ;;
  *)
    echo "unsupported architecture for Codex install: $(uname -m)"
    exit 1
    ;;
esac

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

attempt=1
while :; do
  echo "installing Codex from the GitHub release (attempt $attempt of 3)..."
  if curl -fsSL "https://github.com/openai/codex/releases/latest/download/codex-$target.tar.gz" -o "$scratch/codex.tar.gz" &&
    tar -xzf "$scratch/codex.tar.gz" -C "$scratch"; then
    mkdir -p "$HOME/.local/bin"
    cp "$scratch/codex-$target" "$HOME/.local/bin/codex"
    chmod 0755 "$HOME/.local/bin/codex"
    if "$HOME/.local/bin/codex" --version >/dev/null 2>&1; then
      echo "codex installed: $("$HOME/.local/bin/codex" --version)"
      grep -qs '\.local/bin' "$HOME/.profile" ||
        printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >>"$HOME/.profile"
      exit 0
    fi
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
