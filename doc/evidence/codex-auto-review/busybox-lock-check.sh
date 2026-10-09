#!/bin/sh
set -eu
export HOME=/tmp/codex-lock-fixture
mkdir -p "$HOME/bin"
export PATH="$HOME/bin:$PATH"
export CODEX_HOME="$HOME/config"
printf '#!/bin/sh\necho codex-fixture\n' > "$HOME/bin/codex"
printf '#!/bin/sh\ntouch "$HOME/retried"\n' > "$HOME/bin/sleep"
chmod +x "$HOME/bin/codex" "$HOME/bin/sleep"
printf 'BusyBox version: '
/bin/busybox | head -n 1
printf '\nFree lock: '
sh /evidence/codex-review-script.sh
lock="$CODEX_HOME/.fleet-codex.lock"
[ -f "$lock" ]
inode=$(stat -c %i "$lock")
printf '\nHeld lock: '
exec 8<>"$lock"
flock -n 8
printf 'approvals_reviewer = "user"\n' > "$CODEX_HOME/config.toml"
cp "$CODEX_HOME/config.toml" "$HOME/original.toml"
status=0
sh /evidence/codex-review-script.sh > "$HOME/result" 2>&1 || status=$?
cat "$HOME/result"
[ "$status" = 1 ]
grep -Fq "timed out waiting for Codex config lock: $lock" "$HOME/result"
cmp "$HOME/original.toml" "$CODEX_HOME/config.toml"
[ -f "$HOME/retried" ]
flock -u 8
printf '\nReleased lock: '
sh /evidence/codex-review-script.sh
[ "$(stat -c %i "$lock")" = "$inode" ]
printf '\nReal BusyBox error (close fd 9 in flock wrapper): '
cat > "$HOME/bin/flock" <<'WRAPPER'
#!/bin/sh
exec 9>&-
exec /bin/busybox flock "$@"
WRAPPER
chmod +x "$HOME/bin/flock"
rm "$HOME/retried"
cp "$CODEX_HOME/config.toml" "$HOME/original.toml"
status=0
sh /evidence/codex-review-script.sh > "$HOME/result" 2>&1 || status=$?
cat "$HOME/result"
[ "$status" = 1 ]
grep -Fq "cannot lock Codex config: $lock" "$HOME/result"
grep -Fq 'Bad file descriptor' "$HOME/result"
[ ! -e "$HOME/retried" ]
cmp "$HOME/original.toml" "$CODEX_HOME/config.toml"
printf '\nPASS: free/held/released locks and BusyBox exit-1 errors; error has no retry and preserves config.\n'
