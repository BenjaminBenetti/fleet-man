# fleet-man integration tests

End-to-end tests exercising the `fleet` CLI against the devcontainer
backend. Each test drives the real binary against a fixture repo and a
real docker daemon.

## Layout

```
integration/
├── run.sh              # driver: builds binary, runs every test, teardown between
├── common.sh           # shared helpers: assertions, setup/teardown, fleet_up
├── fixture/            # minimal devcontainer fixture cloned via file:// per test
│   └── .devcontainer/
│       └── devcontainer.json
└── tests/
    ├── 010_up_and_ls.sh        # CLI — up + ls
    ├── 020_exec.sh             # CLI — exec
    ├── 030_stop_start.sh       # CLI — stop / start
    ├── 040_status.sh           # CLI — status
    ├── 050_down.sh             # CLI — down
    ├── 060_destroy.sh          # CLI — destroy
    ├── 100_tui_startup.sh      # TUI — launch + render
    ├── 110_tui_collapse.sh     # TUI — space collapses / expands fleet
    ├── 120_tui_stop_start.sh   # TUI — s toggles instance status
    ├── 130_tui_delete_cancel.sh# TUI — d opens dialog, n cancels
    ├── 140_tui_quit.sh         # TUI — q exits cleanly
    └── 150_tui_refresh.sh      # TUI — r re-reads state
```

TUI tests drive the real `fleet` binary inside a detached `tmux` session
using `send-keys` and read back rendered state via `capture-pane` —
see `common.sh` `tui_*` helpers.

## Contract

Every test file is standalone and owns its own result row. Adding a new
test means dropping a file in `tests/` — nothing to edit elsewhere.

A test file follows this shape:

```bash
#!/usr/bin/env bash
# Description: <one-line description shown in the Actions summary>
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }   # optional — only if the test needs cleanup
itest_begin

setup_test
# ...drive fleet / TUI, run assert_* / tui_assert_*...
pass "short summary"
```

1. `# Description:` — grepped straight out of the file.
2. `itest_cleanup` — optional hook, runs before the result row is emitted.
3. `itest_begin` — records start time and installs an EXIT trap that
   writes `name|status|duration|description` to the shared results file.
4. `setup_test` — wipes `~/.fleet` and re-creates the fixture git repo.
5. `pass` / `fail` — print and emit the row; test exits. If the script
   crashes before either fires, the EXIT trap emits a `fail` row so the
   runner never silently loses a test.

`run.sh` does not judge results — it just runs each file, then
aggregates the rows into a terminal summary and the `$GITHUB_STEP_SUMMARY`
markdown table.

`run.sh` calls `teardown_test` after every test to nuke containers and
state so the next test starts clean.

## Prerequisites

- Go toolchain (for building `fleet`)
- Docker daemon reachable (`docker info` works)
- `devcontainer` CLI (`npm install -g @devcontainers/cli`)
- `tmux` (used by the TUI tests as a headless terminal)

## Running

```
./integration/run.sh
```

Use a prebuilt binary:

```
FLEET_BIN=/path/to/fleet ./integration/run.sh
```

## CI

Two PR checks run this suite end-to-end on every PR to `main`:

- `.github/workflows/integration.yml` — Linux (`ubuntu-latest`), the
  primary target.
- `.github/workflows/integration-windows.yml` — Windows host running
  Ubuntu-24.04 inside WSL2, with dockerd installed in the distro.
  Fleet is intended to run inside WSL on Windows (not native Windows),
  so this job exercises that path.
