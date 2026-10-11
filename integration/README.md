# fleet-man integration tests

End-to-end tests exercising the `fleet` CLI against a real backend:
devcontainer by default, or a Kubernetes cluster (see
[Backends and markers](#backends-and-markers)). Each test drives the real
binary against a fixture repo and a real docker daemon.

## Layout

```
integration/
├── run.sh                      # driver: builds binary, runs every test, teardown between
├── common.sh                   # shared helpers: assertions, setup/teardown, fleet_up,
│                               # backend-neutral instance helpers, test markers
├── backends/                   # per-backend side of those helpers (FLEET_ITEST_BACKEND)
│   ├── devcontainer.sh
│   └── kubernetes.sh
├── fixture/                    # minimal devcontainer fixture git-cloned (local path) per test;
│                               # also fed as a file:// template by the template-fleet tests
│   └── .devcontainer/
│       └── devcontainer.json
├── fixture-no-devcontainer/    # repo template deliberately missing a devcontainer
│                               # config, used by new-fleet check-flow tests
├── fixture-launch/             # devcontainer with a customizations.fleet.fleetLaunch
│                               # block (Links + Apps), used by the launch tests
├── fixture-net/                # itest-net.pl (in-instance HTTP/WebSocket/TCP servers),
│                               # socat, and a Fleet Launch block pointing at them
├── fixture-hostsock/           # binds the host's SSH_AUTH_SOCK and gpg-agent sockets
├── fixture-hostgw/             # --add-host=host.docker.internal:host-gateway
├── fixture-vscode/             # customizations.vscode, postAttachCommand, forwardPorts,
│                               # portsAttributes
├── net/                        # net.sh (itest_net_* helpers) + client.py, the host end
│                               # of the forwarding / browser-proxy tests
├── fixture-lifecycle/          # every lifecycle hook logs its name (hook counts)
├── fixture-dockerfile/         # Dockerfile build (args, repo-root context) + a local
│                               # feature with an option, containerEnv/remoteEnv
├── fixture-go-feature/         # the published Go feature (debug grants, init)
├── fixture-dind/               # the published docker-in-docker feature
├── fixture-hostbind/           # a read-write dir and a read-only file host bind
├── poison-docker/docker        # docker stand-in that logs or refuses calls (itest_poison_docker)
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

## Backends and markers

`FLEET_ITEST_BACKEND` picks the backend instances are provisioned on:
`devcontainer` (the default) or `kubernetes`, with `FLEET_ITEST_CLUSTER`
naming the kubeconfig context (default: the current one; run.sh checks it
answers before the first test). On kubernetes each test's `setup_test`
connects that context to the fresh `~/.fleet` as the connection `itest`, in
`FLEET_ITEST_NAMESPACE` (default `fleet-itest`, created when missing), makes
it the remembered deploy target (so `fleet up`, MCP fleet_up and the TUI's
New instance dialog create on it), and waits until fleetd judges it usable.
fleetd serves its pods the pod build of the fleet under test, which
`FLEET_POD_BINARY_<ARCH>` names (run.sh builds one beside `FLEET_BIN` when it
builds that too); `FLEET_POD_TMUX_<ARCH>` names the tmux bundle images without
tmux need. Teardown deletes what is left of the test's connection by label
and stops the test's daemons. The control-dir helpers look in the pod's own
`/fleet-mounts/control` through `fleet exec`; the share sync (P4.6) still
fails the test as "not wired yet", naming the unit that wires it.

Tests other than devcontainer-only ones never inspect a backend directly (no
`docker inspect`, `docker exec`, `kubectl`, or
`~/.fleet/workspaces/<fleet>/<instance>/...` host paths). They use the neutral
helpers in `common.sh`, which dispatch to `backends/<backend>.sh`:

| Helper | What it checks or does |
| --- | --- |
| `itest_container_id [<ref>]`, `itest_container_ids` | container ids fleet recorded in state.json |
| `itest_container_status`, `itest_container_exists`, `assert_container_running` / `_stopped` / `_absent` | the container behind an id |
| `itest_container_belongs_to`, `itest_container_leftovers` | the identity fleet matches an instance's container by, and containers left for it |
| `itest_container_logs`, `itest_container_stop`, `itest_container_exec_as` | logs, a stop behind fleet's back, a command as another user |
| `itest_container_label` | a label `devcontainer up` stamped on the container (e.g. `devcontainer.metadata`) |
| `itest_workspace_exists`, `assert_workspace_exists` / `_absent`, `itest_workspace_test`, `_read`, `_write`, `_git` | the instance's checkout |
| `itest_control_test`, `_read`, `_mode` | the instance's `/fleet-mounts/control` dir |
| `itest_share_dir`, `itest_share_sync` | the fleet's shared-mount tree on the daemon host, and waiting for it to sync |
| `itest_install_local_deb`, `itest_local_deb_status` | apt-install a one-file package built inside the instance (no network), and whether it is still installed |
| `itest_poison_docker`, `itest_poison_docker_saw`, `itest_poison_docker_log` | put the docker stand-in on the daemon's PATH (`record` or `poison`), and read what it saw |
| `itest_backend_is` | for the rare step with no counterpart elsewhere (a host bind) |
| `itest_backend_rebuilds` | the backend can rebuild an instance in place: a test's rebuild steps go inside it |
| `itest_config_written` | after a test writes `~/.fleet/config.json` itself: creates still land on the backend under test, and the next daemon starts with that config |
| `itest_row_target` | what a TUI instance row shows beside its branch (`⬡`, `☸ itest`) |

`<ref>` is `<fleet>/<instance>`, as `fleet exec` takes it. A step that only
makes sense on one backend goes inside `if itest_backend_is devcontainer`, with
a comment saying why.

To drive the daemon's MCP tools, `itest_mcp_call <tool> <arguments-json>
[<field>]` calls one tool on the local MCP endpoint and prints its result (or
one field of it); `itest_json_field <path>` reads a field out of JSON on stdin.

`itest_poison_docker` puts `poison-docker/docker` first on PATH and restarts
the daemon so it (and the devcontainer CLI it runs) calls docker through it.
`record` passes every call through; `poison` refuses the container verbs (run,
exec, start, stop, ps, logs, inspect of a container, ...) with exit code 125 and
lets image, build and version calls through. Each call is logged on one line as
`pass|refused <args>`. A cluster backend runs its lifecycle with docker
poisoned, so treating an instance as a docker container fails loudly;
`tests/726_poison_docker.sh` is the lifecycle check and
`tests/728_poison_docker_shim.sh` tests the shim itself.

`# itest:` lines in a test file mark how it relates to the backends:

| Marker | Meaning |
| --- | --- |
| `# itest: no-docker` | host-side only (no instance); `FLEET_ITEST_NO_DOCKER=1` runs just these |
| `# itest: k8s` | runs on `FLEET_ITEST_BACKEND=kubernetes`; the unit that ports a feature adds it to that feature's tests |
| `# itest: devcontainer-only: <reason>` | checks a Docker mechanism clusters lack, so the kubernetes run skips it; the reason says what replaces it. These must reach 0 before the kubernetes backend merges |

An unmarked test that creates instances is not ported to kubernetes yet; the
kubernetes run leaves it out. run.sh fails a test whose markers are malformed
(unknown marker, an exemption without a reason, or `k8s` plus
`devcontainer-only`) without running it. Every run's summary ends with the
k8s parity of the selected tests (marked `k8s`, devcontainer-only, not ported
yet, host-only) and lists each exemption with its reason.

Any line that starts with `# itest:` counts, heredocs included, so a test that
writes such a line for another file builds it with `printf`.
`tests/005_itest_harness.sh` covers the markers, the helpers and run.sh's
kubernetes selection.

The `8xx_k8s_*` tests check what only a cluster has (a fleetd stopped
mid-seed, a substituted pod, a crash loop parked, a cut to the API server, a
restricted namespace, a compose refusal, exec upgrades). They are marked
`k8s` and pass at once on devcontainer, saying so. They reach the cluster
through the `itest_k8s_*` helpers of `backends/kubernetes.sh` (kubectl on the
run's context and namespace, the objects of an instance key, its pod).

## Prerequisites

- Go toolchain (for building `fleet`)
- Docker daemon reachable (`docker info` works)
- `devcontainer` CLI (`npm install -g @devcontainers/cli`)
- `tmux` (used by the TUI tests as a headless terminal)
- `perl` (the state.json readers use its core JSON::PP)
- for `FLEET_ITEST_BACKEND=kubernetes`: `kubectl` and a reachable cluster

## Running

```
./integration/run.sh
```

Use a prebuilt binary:

```
FLEET_BIN=/path/to/fleet ./integration/run.sh
```

On a cluster (only the `# itest: k8s` tests run; run.sh builds the pod build
too, or with a prebuilt `FLEET_BIN` takes it from `FLEET_POD_BINARY_<ARCH>`):

```
FLEET_ITEST_BACKEND=kubernetes FLEET_ITEST_CLUSTER=minikube ./integration/run.sh
```

Inside a fleet instance, run the suite in a QA container instead; see
DEVELOPMENT.md "Integration tests" and, for a private cluster, "Dev loop".

## CI

`.github/workflows/integration-k8s.yml` runs the kubernetes subset sharded on
a minikube in each runner, beside the backend conformance suite and the
cluster-gated Go tests (non-required until the Kubernetes backend merges).

Two PR checks run this suite end-to-end on every PR to `main`:

- `.github/workflows/integration.yml` — Linux (`ubuntu-latest`), the
  primary target.
- `.github/workflows/integration-windows.yml` — Windows host running
  Ubuntu-24.04 inside WSL2, with dockerd installed in the distro.
  Fleet is intended to run inside WSL on Windows (not native Windows),
  so this job exercises that path.
