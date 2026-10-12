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
├── editor/                     # editor.sh (itest_editor_* helpers): ssh -F through
│                               # ~/.ssh/fleet_config, a gateway as the remote daemon,
│                               # a fake VS Code server for the Remote-SSH tests
├── fixture-lifecycle/          # every lifecycle hook logs its name (hook counts)
├── fixture-dockerfile/         # Dockerfile build (args, repo-root context) + a local
│                               # feature with an option, containerEnv/remoteEnv
├── fixture-go-feature/         # the published Go feature (debug grants, init)
├── fixture-dind/               # the published docker-in-docker feature
├── fixture-buildx/             # the docker CLI + buildx and no daemon: builds go
│                               # to the fleet's shared buildkit server
├── fixture-hostbind/           # a read-write dir and a read-only file host bind
├── fixture-volume/             # a named volume over /etc/skel (copied up, shared)
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
`FLEET_ITEST_NAMESPACE` (default `fleet-itest`, created when missing; with
the settings `FLEET_ITEST_CLUSTER_FIELDS` gives, as `fleet cluster add` takes
them, e.g. `storage_class=csi-hostpath-sc`), makes
it the remembered deploy target (so `fleet up`, MCP fleet_up and the TUI's
New instance dialog create on it), and waits until fleetd judges it usable.
fleetd serves its pods the pod build of the fleet under test, which
`FLEET_POD_BINARY_<ARCH>` names (run.sh builds one beside `FLEET_BIN` when it
builds that too); `FLEET_POD_TMUX_<ARCH>` names the tmux bundle images without
tmux need. Teardown deletes what is left of the test's connection by label
and stops the test's daemons. The control-dir helpers look in the pod's own
`/fleet-mounts/control` through `fleet exec`. A fleet's shared mounts live on
its share in the cluster, which fleetd keeps converged with the host tree;
`itest_share_sync` waits for that with `fleet sync wait` (bounded by
`FLEET_ITEST_SHARE_SYNC_TIMEOUT`, 120s) and fails the test, naming what was
behind, when it does not converge.

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
| `itest_service_status`, `itest_service_remove`, `itest_wait_service` | a fleet's shared service server (`buildkit`, `debcache`, `imagecache`): a host container, or a Deployment on a cluster |
| `itest_install_local_deb`, `itest_local_deb_status` | apt-install a one-file package built inside the instance (no network), and whether it is still installed |
| `itest_poison_docker`, `itest_poison_docker_saw`, `itest_poison_docker_log` | put the docker stand-in on the daemon's PATH (`record` or `poison`), and read what it saw |
| `itest_backend_is` | for the rare step with no counterpart elsewhere (a host bind) |
| `itest_backend_rebuilds` | the backend can rebuild an instance in place: a test's rebuild steps go inside it |
| `itest_config_written` | after a test writes `~/.fleet/config.json` itself: creates still land on the backend under test, and the next daemon starts with that config |
| `itest_row_target` | what a TUI instance row shows beside its branch (`⬡`, `☸ itest`) |

`<ref>` is `<fleet>/<instance>`, as `fleet exec` takes it. A step that only
makes sense on one backend goes inside `if itest_backend_is devcontainer`, with
a comment saying why.

The VS Code Remote-SSH tests source `editor/editor.sh`: `itest_editor_ssh`
and `itest_editor_sftp` connect through the `~/.ssh/fleet_config` that `fleet
code` writes (`ssh -F`, no remote-daemon variables), with `~/.ssh` set aside
for the test (`itest_editor_isolate_ssh`); `itest_editor_gateway_start` /
`_url` make the test's daemon a remote one behind a local gateway; and
`itest_editor_fake_vscode` / `_vscode_attach` stand in for VS Code's server
and a window's connection to it.

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
restricted namespace, a compose refusal, exec upgrades, a fleet's own
objects, the cloud metadata block, the image mirror in a pod's own docker
engine). They are marked `k8s` and pass at once on
devcontainer, saying so. They reach the cluster through the `itest_k8s_*`
helpers of `backends/kubernetes.sh` (kubectl on the run's context and
namespace, the objects of an instance key, its pod, a fleet's objects).
`tests/811_k8s_metadata_block.sh` needs a network plugin that enforces
NetworkPolicies (`FLEET_QA_MINIKUBE_FLAGS=--cni=calico`) and passes saying so
on one that does not. `tests/818_k8s_share_single_node.sh` (a fleet's pods
placed by its share) needs two schedulable nodes and a WaitForFirstConsumer
default class, as a kind cluster with two workers has (`kind create cluster
--config` in the QA container, then `FLEET_ITEST_CLUSTER=kind-<name>`); on one
node it passes saying it did not run. `tests/819_k8s_evacuate_drain.sh` (a
fleet moved off a drained node, and one on a node-local class left there)
also needs a class whose volumes every node mounts
(`FLEET_ITEST_MOVABLE_CLASS`, default `fleet-shared-path`); since that class
passes the multi-node test too, it runs its fleetd with
`FLEET_KUBE_MULTI_NODE=off`, which keeps new shares in single-node mode.
`clusters/kind-shared-path.sh up [name] [workers]` makes such a cluster in
the QA container (two nodes that both take pods, kind's node-local default
class, and `fleet-shared-path` on a host folder every node mounts) and prints
its context; `down [name]` deletes it:

```bash
ctx=$(./integration/clusters/kind-shared-path.sh up p405)
FLEET_ITEST_BACKEND=kubernetes FLEET_ITEST_CLUSTER="${ctx}" FLEET_ITEST_RANGE=818-820 ./integration/run.sh
./integration/clusters/kind-shared-path.sh down p405
```

`tests/823_k8s_share_multi_node.sh` (multi-node mode: Auto's tested
ReadWriteMany class, siblings on two nodes) needs two schedulable nodes and a
StorageClass of csi-driver-nfs (`nfs.csi.k8s.io`): a kind cluster with two
workers, an NFS server and the driver installed (its manifests, and
`deploy/example/nfs-provisioner` for the server, on a tmpfs volume: an overlay
folder cannot be exported); elsewhere it passes saying it did not run.
`tests/825_k8s_attach_limit.sh` (the fleet-size cap of a node's volume attach
limit, and the instance past it Pending with the limit named) needs a
csi-hostpath driver that takes few volumes per node:
`clusters/csi-hostpath-limit.sh <context> [limit]` sets one on a throwaway
cluster with the driver (in the QA container,
`FLEET_QA_MINIKUBE_FLAGS="--memory=6g --cpus=4 --addons=csi-hostpath-driver"`,
then `clusters/csi-hostpath-limit.sh minikube 3`); elsewhere it passes saying
it did not run. `tests/826_k8s_share_move.sh` (a stopped fleet's share moved
to a new shared StorageClass) needs a second class to move the share to
(`FLEET_ITEST_SHARE_TO_CLASS`, else an `nfs.csi.k8s.io` class,
`fleet-shared-path` or a `hostpath.csi.k8s.io` one): on the minikube above it
moves `standard` to `csi-hostpath-sc`, on the NFS kind cluster to the NFS
class (multi-node mode; `FLEET_ITEST_SHARE_MOVE_MODE=multi-node` asserts it).

## Prerequisites

- Go toolchain (for building `fleet`)
- Docker daemon reachable (`docker info` works)
- `devcontainer` CLI (`npm install -g @devcontainers/cli`)
- `tmux` (used by the TUI tests as a headless terminal)
- `perl` (the state.json readers use its core JSON::PP)
- for `FLEET_ITEST_BACKEND=kubernetes`: `kubectl` and a reachable cluster
- for `203_tui_kube_settings_ssh.sh` (a TUI over Armada SSH): Linux and
  passwordless sudo. For the test's length it adds a throwaway user and runs an
  sshd on a free loopback port (the QA image has openssh-server; elsewhere it is
  apt-installed); without them the test passes saying it did not run

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

`FLEET_ITEST_ARTIFACTS=<dir>` keeps what a failed test leaves before its
teardown wipes it, in `<dir>/<test>/`: its daemon's `fleet.log` and
`state.json`, and the backend's state (`_be_dump`: the host's containers and
fleet's container logs, or the test namespace's objects, events, pods and pod
logs, the nodes and the storage).

## Nightly jobs

`integration/nightly/*.sh` are what `.github/workflows/nightly-k8s.yml` runs
(DEVELOPMENT.md "Nightly jobs" has the local recipe). Each makes its own
throwaway cluster, runs the itests one minikube node cannot (most of them pass
saying they did not run elsewhere), and leaves `summary.md`, `budgets.tsv`,
the itest logs, failed tests' `FLEET_ITEST_ARTIFACTS` and cluster dumps in
`$FLEET_NIGHTLY_OUT/<job>/`:

| Job | Cluster | Tests |
| --- | --- | --- |
| `kind-multinode.sh` | kind, three nodes (`clusters/kind-shared-path.sh`) | 817-822 (share placement, co-location, Immediate refusal, a drain with a TUI watching, rebuilds), 852 (the deep test's two-pod probe, the one-node warning and cap) |
| `kind-nfs.sh` | kind, two nodes, csi-driver-nfs and an NFS server on a tmpfs | 817-827 onto `nfs-csi` (823: multi-node mode), 826 onto `fleet-shared-path` in multi-node mode; the NFS-safe teardown order |
| `selinux-k3s.sh` | k3s `--selinux` on an enforcing AlmaLinux 9 VM (KVM) | 817, 824, 853 (one MCS level per fleet, siblings sharing files, the relabel time) |
| `calico.sh` | minikube, calico at a 1450-byte MTU | 714-718 (DinD at that MTU, the deb and image caches), 808-815 (811: the metadata block calico enforces) |
| `csi-hostpath.sh` | minikube, volumesnapshots + csi-hostpath | 700, 808, 812, 391, 780 on `csi-hostpath-sc` (snapshot clones); the conformance suite on `standard` and `csi-hostpath-sc`; `TestLiveSeedCache` |
| `attach-limit.sh` | minikube, csi-hostpath at 3 volumes per node | 825-826 |
| `expand.sh` | minikube, OpenEBS LVM LocalPV on a loop-file volume group (`clusters/lvm-localpv.sh`): volumes that hold only their size and grow | 854: a share that fills, pauses the sync and grows with auto-expand on; a cache volume growing |
| `runtime-docker.sh` | minikube `--container-runtime=docker` (the shards' minikube runs containerd) | 010-061, 800-807 |
| `rbac-limited.sh` | minikube; the connection a ServiceAccount token whose Role, in its namespace, grants only what the preflight requires, and nothing cluster-wide | 010-061, 800 |
| `netem.sh` | minikube behind tc netem (150ms round trip; then with ~1% loss) | 851: audio jitter beside bulk transfers, page loads through fleet vs kubectl port-forward, poll times |
| `scale.sh <n>` | minikube, one node of `n`+60 pods | 850: fleetd's scale budgets and the reconnect storm at `n` (100; 300 on a larger runner) |
| `poll-cost.sh` | none (macOS in the workflow) | a poll pass over 20 bound files in a 5000-entry host folder under 100ms (`BenchmarkPollOfBoundFiles`): sides without inotify poll every 2s |
| `go-deep.sh` | none | sharesync properties at 200 seeds, every fuzz target for a minute |

The budgets the 85x tests hold (each env-overridable there) come from the
plan's scale and WAN budgets: fleetd under 600 MB, under 4 open files and at
most 2 API server long-running requests per pod, every agent back within 90s
of a restart; PCM jitter p99 under 60ms beside a 48MB copy and a 50MB synced
file on a lossless 150ms link (recorded, not judged, with loss), and a page
through fleet within 2x of kubectl port-forward on the same link.

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
