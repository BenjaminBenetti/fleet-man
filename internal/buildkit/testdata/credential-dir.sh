set -e; umask 077; d="$HOME/.fleet/buildkit"; mkdir -p "$d"; chmod 700 "$d"; printf 'FLEET_CREDENTIAL_DIR=%s\n' "$d"
