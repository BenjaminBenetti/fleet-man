#!/bin/sh
# Run only in a disposable Fleet fixture instance: briefly hide its runtime
# config to compare the same sandbox command before and after provisioning.
set -eu
config_home="${CODEX_HOME:-$HOME/.codex}"
config="$config_home/config.toml"
backup=$(mktemp "$config_home/.sandbox-probe-config.XXXXXX")
cp -p "$config" "$backup"
trap 'mv -f "$backup" "$config"' EXIT
trap 'exit 1' HUP INT TERM
rm "$config"
probe='printf "sandbox-write-ok\n" > .codex-sandbox-probe; cat .codex-sandbox-probe; rm .codex-sandbox-probe'
printf 'Codex version: '
codex --version
printf 'Workspace: '
pwd
printf '\nBaseline: no Fleet-generated config (pre-PR behavior)\n'
baseline=0
codex sandbox -- sh -ec "$probe" || baseline=$?
printf 'baseline exit=%s\n' "$baseline"
cp -p "$backup" "$config"
printf '\nPR: workspace-write and auto_review\n'
configured=0
codex sandbox -- sh -ec "$probe" || configured=$?
printf 'PR exit=%s\n' "$configured"
