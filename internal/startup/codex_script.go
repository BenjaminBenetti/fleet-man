package startup

// codexScript installs the complete standalone package, including the Code Mode
// host and bundled resources, using OpenAI's official installer. A bare release
// binary can answer --version while failing to start Code Mode.
//
// CODEX_HOME is overridden only for the installer: packages and install locks
// stay container-local instead of racing on the fleet's shared ~/.codex mount.
// Runtime config and authentication still use the normal Codex home. Provisioning
// sets the default permissions to workspace-write with automatic approval review.
// Existing image-provided installs are preserved; incomplete Fleet installs in
// ~/.local/bin are repaired. Installation needs no Node.js or npm.
func codexScript() Script {
	return Script{
		Name: "codex",
		Body: `launcher="$HOME/.local/bin/codex"
install_home="$HOME/.local/share/fleet/codex"
package="$install_home/packages/standalone/current"

# Configure even an image-provided or already complete install. Use a shared
# kernel lock and an atomic rename because instances can provision together.
configure_auto_mode() (
  config_home="${CODEX_HOME:-$HOME/.codex}"
  mkdir -p "$config_home" || exit 1
  config="$config_home/config.toml"
  links=0
  while [ -L "$config" ]; do
    links=$((links + 1))
    if [ "$links" -gt 40 ]; then
      echo "too many symlinks resolving Codex config"
      exit 1
    fi
    target=$(readlink "$config") || exit 1
    case "$target" in
      /*) config="$target" ;;
      *) config="$(dirname "$config")/$target" ;;
    esac
  done
  config_dir=$(dirname "$config")
  mkdir -p "$config_dir" || exit 1
  if [ ! -w "$config_dir" ]; then
    echo "cannot configure Codex auto mode: config directory is not writable: $config_dir"
    exit 1
  fi
  if ! command -v flock >/dev/null 2>&1; then
    echo "flock is required to configure Codex auto mode (install util-linux)"
    exit 127
  fi
  # Keep a stable lock file open for writing, including for NFS flock support.
  # The kernel releases the lock on SIGKILL; never unlink the lock file because
  # concurrent waiters must continue to share the same inode.
  lock="$config_dir/.fleet-codex.lock"
  exec 9<>"$lock"
  tries=0
  while :; do
    lock_err=$(flock -n 9 2>&1) && break
    status=$?
    # BusyBox uses exit 1 for errors as well as contention. Both supported
    # implementations are silent on contention, so preserve other diagnostics.
    if [ "$status" -ne 1 ] || [ -n "$lock_err" ]; then
      echo "cannot lock Codex config: $lock${lock_err:+ ($lock_err)}"
      exit "$status"
    fi
    tries=$((tries + 1))
    if [ "$tries" -ge 30 ]; then
      echo "timed out waiting for Codex config lock: $lock"
      exit 1
    fi
    sleep 1
  done
  config_tmp=""
  trap 'rm -f "$config_tmp"' EXIT
  trap 'exit 1' HUP INT TERM
  config_tmp=$(mktemp "$config_dir/.config.toml.XXXXXX") || exit 1
  source=/dev/null
  if [ -e "$config" ]; then
    source="$config"
    cp -p "$config" "$config_tmp" || exit 1
  fi
  # Defaults belong before TOML tables. Only replace these top-level settings;
  # nested profiles and unrelated user settings retain their values.
  awk '
    BEGIN {
      print "approval_policy = \"on-request\""
      print "approvals_reviewer = \"auto_review\""
      print "sandbox_mode = \"workspace-write\""
      key = "(approval_policy|approvals_reviewer|sandbox_mode)"
      setting = "(" key "|\"" key "\"|\047" key "\047)"
    }
    {
      # Strings and continued arrays/inline tables may contain text that looks
      # like settings or table headers. Locate only actual TOML expressions.
      if (multiline == "" && depth == 0) {
        skip_value = 0
        if ($0 ~ /^[[:space:]]*\[/) {
          in_table = 1
          skip_table = ($0 ~ "^[[:space:]]*\\[[[:space:]]*" setting "[[:space:]]*(\\.|\\])")
        } else if (!in_table && $0 ~ "^[[:space:]]*" setting "[[:space:]]*(=|\\.)") {
          skip_value = 1
        }
      }
      if (!skip_value && !skip_table) print
      quote = ""
      for (i = 1; i <= length($0); i++) {
        c = substr($0, i, 1)
        triple = substr($0, i, 3)
        if (multiline != "") {
          if (c == "\\" && substr(multiline, 1, 1) == "\"") { i++; continue }
          if (triple == multiline) {
            i += 2
            while (substr($0, i + 1, 1) == c) i++
            multiline = ""
          }
        } else if (quote != "") {
          if (c == "\\" && quote == "\"") { i++; continue }
          if (c == quote) quote = ""
        } else if (c == "#") {
          break
        } else if (c == "\"" || c == "\047") {
          if (triple == c c c) { multiline = triple; i += 2 }
          else quote = c
        } else if (c == "[" || c == "{") {
          depth++
        } else if (c == "]" || c == "}") {
          depth--
        }
      }
    }
  ' "$source" >"$config_tmp" || exit 1
  if [ -f "$config" ] && cmp -s "$config" "$config_tmp"; then
    echo "codex auto mode already configured"
    exit 0
  fi
  mv -f "$config_tmp" "$config" || exit 1
  echo "codex configured for auto mode (automatic approval review)"
)

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
  configure_auto_mode
  exit $?
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
    configure_auto_mode
    exit $?
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
