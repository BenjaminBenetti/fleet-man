#!/usr/bin/env bash
set -eu
PS4='$ '
set -x
fleet exec codex-auto-evidence/auto-default -- bash -lc 'codex --version; codex login status || true'
python3 /tmp/fleet-codex-evidence/read-mode.py
