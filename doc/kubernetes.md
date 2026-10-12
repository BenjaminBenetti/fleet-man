# Fleet on Kubernetes

A fleet instance can run on a Kubernetes cluster instead of the daemon host's
Docker. Sessions, shells, `fl`/`fc`/`fo`, port forwards, the browser, the
microphone and audio output, Fleet MCP, the status mod, automation and VS Code
work as they do on Docker. This page covers what is different: what a cluster
needs, how fleet connects to it, what it puts there, and the trust model.

> For the short version, see [Kubernetes](../README.md#kubernetes) in the
> README. This page is the reference.

## Contents

1. [How it works](#1-how-it-works)
2. [What you need](#2-what-you-need)
3. [Connecting a cluster](#3-connecting-a-cluster)
4. [Creating instances](#4-creating-instances)
5. [Connection settings](#5-connection-settings)
6. [Storage](#6-storage)
7. [Shared folders, logins and sync](#7-shared-folders-logins-and-sync)
8. [Networking](#8-networking)
9. [Docker, Buildkit and the caches](#9-docker-buildkit-and-the-caches)
10. [Audio](#10-audio)
11. [Lifecycle](#11-lifecycle)
12. [Automation](#12-automation)
13. [VS Code](#13-vs-code)
14. [Security and trust model](#14-security-and-trust-model)
15. [Environment variables](#15-environment-variables)
16. [Known differences from Docker](#16-known-differences-from-docker)

## 1. How it works

- **An instance is a StatefulSet with one pod**, `<sts>-0`, in the
  connection's namespace. Its root file system lives on a PersistentVolumeClaim
  of its own (the *instance disk*, 50Gi by default), so what you install
  survives a stop, a restart and a node move, as a Docker container's writable
  layer does.
- **The image is built on the daemon host** by the devcontainer CLI and the
  host's Docker, as for a Docker instance, then delivered to the cluster
  (loaded onto a local cluster's nodes, pushed to a registry, or served
  through the Fleet gateway; an image fleet did not change is pulled from its
  own registry). A seed Job copies the image root and the workspace onto the
  disk, then the StatefulSet starts the pod.
- **fleetd talks to one agent per pod** over the API server (`pods/exec` for
  the agent session, `pods/portforward` for TCP). The agent is fleet's own
  binary, staged into the pod; its protocol is version 2. Before a pod gets a
  session, a file or a credential, fleetd checks it is the pod it made: the
  recorded image digest, a spec hash it recomputes, and the owning StatefulSet.
  A pod that fails is `kubernetes:tampered`.
- **Shared state lives on a fleet share**, one PVC per fleet and cluster,
  kept in sync with the daemon host's `~/.fleet/workspaces/<fleet>`
  ([section 7](#7-shared-folders-logins-and-sync)).
- **Every cluster call runs in fleetd.** Kubeconfigs, cluster tokens and login
  plugins stay on the daemon host. A remote TUI or CLI (gateway or Armada SSH)
  has the same control as a local one and never sees a kubeconfig or token.

Every create is journaled in `~/.fleet/kube/jobs/` before each API write. A
daemon that dies mid-create leaves the instance `failed · interrupted`; one
that dies mid-rebuild has the rebuild rolled back on its next start.

## 2. What you need

**On the daemon host** (where fleetd runs):

- `docker` and the devcontainer CLI. Images are built there; without them every
  cluster Deploy option is unavailable, with the reason.
- A kubeconfig with a context for the cluster, and its exec credential plugin
  (`aws`, `gke-gcloud-auth-plugin`, `kubelogin`, …) on fleetd's `PATH` or a
  connection's `extra_path`. Settings → Tool Status lists each connection's
  plugin, `on fleetd @ <host>`.
- The pod build of fleet (`fleet-pod-linux-<arch>`, no Kubernetes client in
  it) and a static tmux bundle for images without tmux, both of exactly the
  daemon's version, which fleetd stages into pods. A release daemon downloads
  them once from its GitHub release into `~/.fleet/bin`, checked against the
  release's checksums; an offline daemon host needs them there, or named by
  `FLEET_POD_BINARY_<ARCH>` / `FLEET_POD_TMUX_<ARCH>` (`AMD64`, `ARM64`).

**On the cluster**: a namespace (fleet creates the one a connection names
when it does not exist, given `namespaces create`) and these permissions in
it for fleet's identity. `fleet cluster test` lists any that are missing
(`FORBIDDEN`).

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: fleet, namespace: fleet }
rules:
  - apiGroups: [apps]
    resources: [statefulsets, deployments]
    verbs: [create, get, list, watch, patch, delete]
  - apiGroups: [batch]
    resources: [jobs]
    verbs: [create, get, list, watch, patch, delete]
  - apiGroups: [""]
    resources: [pods]
    verbs: [create, get, list, watch, delete]
  - apiGroups: [""]
    resources: [pods/exec, pods/portforward]
    verbs: [create, get]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [""]
    resources: [events, limitranges, resourcequotas]
    verbs: [get, list]
  - apiGroups: [apps]
    resources: [replicasets]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [persistentvolumeclaims, secrets, configmaps, services]
    verbs: [create, get, list, patch, delete]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [create, get, update]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: [create, get, list, patch, delete]
  # Optional: snapshot clones, the exposure scan, live metrics.
  - apiGroups: [snapshot.storage.k8s.io]
    resources: [volumesnapshots]
    verbs: [create, get, list, delete]
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [rolebindings]
    verbs: [list]
  - apiGroups: [metrics.k8s.io]
    resources: [pods]
    verbs: [list]
  # Required only with protect_from_drain.
  - apiGroups: [policy]
    resources: [poddisruptionbudgets]
    verbs: [create, get, list, patch, delete]
```

Optional cluster-scoped reads make fleet smarter about storage and nodes:
StorageClasses (the class picker; without it you type a class name), nodes
(architectures, capacity, cordon/drain moves), CSINodes and VolumeAttachments
(attach limits), PersistentVolumes (a share's topology), CSIDrivers and
VolumeSnapshotClasses (clone tiers), RuntimeClasses (sysbox), and the
`kube-system` namespace (the cluster's identity, so a renamed context can be
re-pointed). `get`, `list` and `watch` on each is enough. `namespaces create`
lets fleet make a connection's namespace when it does not exist; without it
the connection is still saved, and fleet shows the namespace's manifest to
apply as someone who has it.

**Kubernetes versions.** Docker outside of Docker needs 1.29 or later (native
sidecars). On SELinux nodes, volumes are relabelled at mount time where the
volume's CSI driver supports it and the server is 1.33 or later.

## 3. Connecting a cluster

A **connection** is one kubeconfig context and namespace, held by fleetd in
`~/.fleet/kube_clusters.json` (0600, backed up hourly). A second namespace on
the same cluster is another connection.

**In the TUI**, Settings → Kubernetes lists the connections, each with its
health, version and the instances and agents that use it:

- **+ Kubernetes Cluster** opens the picker over the contexts of fleetd's
  kubeconfig (`contexts from fleetd @ <host>`): `/` filters, `n` sets the
  namespace (`fleet` is suggested when the context's is `default`), `ctrl+o`
  reads another kubeconfig on the daemon host, `enter` tests and connects, `c`
  shows the namespace manifest and creates it, `L` runs the context's login
  plugin in this terminal.
- **enter** on a connection opens the cluster editor: every setting in
  [section 5](#5-connection-settings), grouped and folded. Its Status row has
  **[ test ]**, **[ deep test ]** and **[ login on fleetd ]**. Changes apply to
  new instances; Rebuild applies them to existing ones. The fleets' cache
  servers there take a change of what they run with at once (service images,
  cache storage class and sizes, node selector, tolerations, service account,
  memory limit, Allow privileged pods).
- **[ delete ]** disconnects. A connection instances or agents use is kept
  unless you pick **[ Destroy N instances, then disconnect ]** or **[ Forget
  (scale to 0, leave PVCs) ]**, which leaves them in the cluster and fails
  their records `kubernetes:cluster_missing` (you type the name first unless
  the cluster is unreachable). A removal deletes the fleets' objects and cache
  volumes in the namespace only while this daemon holds the connection's owner
  Lease; otherwise (the cluster not reached, another daemon holding it) it
  deletes nothing and says what it left, with the labels to find it by.
- The section's last line is the trust line: who can act inside your instances,
  and who can read your agent logins ([section
  14](#14-security-and-trust-model)).

**From the CLI**, `fleet cluster` does the same (`fleet cluster <cmd> --help`):

```bash
fleet cluster contexts                          # contexts in fleetd's kubeconfig
fleet cluster add prod-eu -n fleet --name prod  # connect a context; a missing namespace is created
fleet cluster add kind-dev -n team --namespace-label pod-security.kubernetes.io/enforce=baseline
fleet cluster ls                                # connections and their health
fleet cluster ls prod                           # warnings and every set field
fleet cluster set prod allow_privileged=true storage_class=gp3
fleet cluster test prod                         # every check (exit 0: ready or limited)
fleet cluster test --deep prod                  # also throwaway-pod probes (minutes)
fleet cluster login prod                        # run the login plugin on fleetd
fleet cluster rm prod [--destroy-instances | --forget]
fleet cluster gc prod [--apply]                 # objects no instance accounts for
```

`add`, and `set` of a namespace or a context, create the namespace when it
does not exist (labelled `app.kubernetes.io/managed-by=fleet`, or with
`--namespace-label`); when fleetd may not create it, the connection is still
saved and the namespace's manifest is printed to apply. Kubeconfig paths,
`extra_path` and `plugin_env` are daemon-host paths and names, whichever
machine you type them on.

**From MCP**, `fleet_cluster_list {cluster?}` and
`fleet_cluster_test {cluster, deep?, storage_classes?, test_storage?}`.

**Health.** fleetd probes each connection every 30s and runs the full checks
on add, edit, test and every 30 minutes; the TUI and `fleet cluster ls` show
the result:

| State | Meaning |
|---|---|
| ready | every check passed |
| limited | usable, with a degraded capability the warnings name (no root, privileged pods not allowed, one node per fleet, quota full, …) |
| unreachable | the API server does not answer |
| unauthorized | the credentials were refused or expired (`credentials expire in X` warns 30 minutes ahead) |
| forbidden | RBAC denies what fleet needs |
| context missing / plugin missing / misconfigured | the kubeconfig, its login plugin, or a setting needs fixing |
| owner conflict | another fleet daemon holds this connection's owner Lease |

Only ready and limited connections take creates. A connection's checks also
decide its capability tier: root or no root (a namespace whose admission
refuses root pods runs every container as one non-root user, with no apt and no
sudo: limited, `restricted: no root (experimental)`), privileged pods, debug
capabilities, storage and image delivery. A connection holding more than 150
instances warns: each adds to fleetd's memory and to its reconnect after a
restart.

**Logging in.** An exec plugin that needs a browser or a device code has two
ways in:

- **[ login on fleetd ]** (`fleet cluster login`) runs the plugin on the daemon
  host and relays only its device-code URL and code to you. It works from any
  client, local or remote.
- **[L] Run login here** runs the plugin in your own terminal. It is offered
  only over the local socket, where your machine is the daemon host: on a
  remote client it would refresh a kubeconfig fleetd never reads.

An SSO login expires, and only a person can complete its device code: until
someone does, the connection is unauthorized, and creates, starts and
reconnects to its pods fail. For a daemon that runs unattended (a server,
automation overnight), a kubeconfig with a ServiceAccount token, or another
credential that needs no person, avoids that, at a price: a credential valid
until you revoke it, held on that host, whose RBAC is the Role above rather
than your own.

## 4. Creating instances

- **TUI:** each connection is a Deploy option `☸ <name>` in the New instance
  dialog, with its health badge; one that is not ready or limited (or lacks the
  daemon host's tools) is listed under the row with the reason instead. The
  target you create on is remembered by the daemon and preselected next time.
- **CLI:** `fleet up <name> --cluster <id|name|context>`, or
  `--backend kubernetes` (the remembered connection, else the only one). With
  neither, `fleet up` uses the daemon's remembered target, which may be a
  cluster; it prints the backend it used.
- **MCP:** `fleet_up {…, cluster?}` or `backend: "kubernetes"`; without either
  the remembered target is used, and the reply's `target` names it.

**What the Deploy row warns about.** The daemon reads the fleet's
devcontainer.json (its newest checkout on the daemon host, else the last
inspection of its remote) and says, for the picked cluster, what a create there
would meet:

- a compose devcontainer: every cluster is unavailable, `compose devcontainers
  are not supported on Kubernetes`;
- Docker-in-Docker, `--privileged` or a docker.sock bind on a cluster that
  allows no privileged pods, runs no root, or (Docker outside of Docker) is
  older than 1.29;
- a prebuilt image (`image` with no Dockerfile and no features) on a cluster
  without root, with audio on: no audio unless the image has PulseAudio and
  ALSA.

These are readings of devcontainer.json alone. The create's own preflight reads
the merged configuration, has the namespace's admission judge a stand-in pod
before anything is built, and refuses with the exact `kubernetes:` code.

**Templates.** A `file://` template fleet runs on clusters too: its folder is
copied on the daemon host, and the seed takes the copy into the pod as it does
a checkout.

**Progress and failure.** The creation log (`L`) shows each step: building,
delivering the image, applying, waiting for the pod (with what it waits on),
seeding, the lifecycle hooks. A failed create keeps its record; **enter** on
the row opens the recovery dialog, which says what happened and links the
setting that fixes it. **R** (`fleet rebuild`, `fleet_rebuild`) retries a
failed or interrupted create: what it left is deleted and the create runs
again. **d** destroys it.

| Error | What to do |
|---|---|
| `kubernetes:unreachable` | check the cluster is up and reachable from the daemon host |
| `kubernetes:unauthorized` | the login expired or its plugin failed: log in again (Settings → Kubernetes → login on fleetd) |
| `kubernetes:forbidden` | grant what `fleet cluster test` lists |
| `kubernetes:admission_denied` | a namespace whose admission allows the pod, or change the devcontainer |
| `kubernetes:quota` | room in the namespace quota or the connection's `max_instances` / `max_total_disk` |
| `kubernetes:unsupported:*` | change the devcontainer, or run it on Docker |
| `kubernetes:misconfigured` | fix the connection setting it names (or the kubeconfig, when the API server's certificate did not verify) |
| `kubernetes:image_build` / `image_push` / `image_pull` | the build, the delivery settings and registry login, the nodes' route to the image |
| `kubernetes:pvc_pending` | the storage class; `kubectl describe pvc` says why |
| `kubernetes:unschedulable` | a node that fits the instance, or smaller requests |
| `kubernetes:crash_loop` / `oomkilled` | `fleet logs`; for OOM raise `memory_limit`, then start it |
| `kubernetes:timeout` | raise the wait's `FLEET_KUBE_*_TIMEOUT` if the cluster is just slow |
| `kubernetes:interrupted` / `rebuild_failed` | `R` to retry, or `d` |
| `kubernetes:tampered` | someone changed the pod or StatefulSet: destroy and recreate |
| `kubernetes:cluster_missing` | reconnect the cluster, or destroy the instance |
| `kubernetes:owner_conflict` | stop the other daemon, or wait for its Lease to expire |

## 5. Connection settings

Every setting lives on the connection (there are no per-fleet cluster
settings). Set them in the cluster editor or with
`fleet cluster set <c> <field>=<value>`; `<field>=` resets one to its default.
Unset means default.

| Group | Field | Default | What it does |
|---|---|---|---|
| identity | `display_name`, `context`, `kubeconfig_paths`, `namespace` | the context's | while instances use the connection, `namespace` cannot change, and `context` and `kubeconfig_paths` only to the same cluster (its `kube-system` UID) |
| | `extra_path`, `plugin_env` | | PATH and environment for the login plugin on the daemon host. `plugin_env` takes names (pass fleetd's value) or `NAME=value`; values are refused for names that look secret |
| instance disks | `storage_class`, `disk_size`, `snapshot_class` | Auto, 50Gi, Auto | [section 6](#6-storage) |
| | `auto_expand_disk` | on | grow a nearly full share or cache volume, or a disk a rebuild needs room on, where the class allows |
| | `clone_method`, `clone_consistency` | auto, auto | [section 11](#11-lifecycle) |
| | `node_local_ack` | off | accept node-local instance disks on a multi-node cluster (otherwise limited) |
| | `backup_workspace_on_stop` | off | not acted on by this version |
| fleet share | `shared_storage_class`, `share_size` | Auto, 10Gi | [section 6](#6-storage) |
| | `share_retention` | delete | `retain` keeps a destroyed fleet's share |
| caches | `cache_storage_class`, `cache_sizes.{debcache,imagecache,buildkit}` | Auto, 10Gi / 20Gi / 20Gi | [section 9](#9-docker-buildkit-and-the-caches) |
| | `service_images.*` | digest-pinned | the cache, Buildkit and DooD sidecar images |
| | `allow_shared_buildkit` | allowed | a permission; the fleet's own Buildkit toggle decides |
| | `delete_cache_volumes_on_destroy` | off | cache volumes are kept for a warm restart |
| | `dind_address_pool` | docker's | Docker-in-Docker networks: `<cidr>` or `base=<cidr>,size=<bits>`, several separated by spaces or `;` |
| | `block_metadata` | on | deny pod egress to 169.254.0.0/16 and fd00:ec2::/64 (DNS excepted) |
| pod | `resources.cpu_request`, `resources.memory_request` | 500m, 2Gi | raised to `hostRequirements` |
| | `memory_limit` | 8Gi | set explicitly when a LimitRange would inject a lower limit |
| | `resources.autopilot_memory` | 4Gi | for GKE Autopilot, which fleet does not detect yet |
| | `node_selector`, `tolerations` | | e.g. a dedicated node pool |
| | `allow_privileged` | auto | on for a detected local cluster (minikube, kind, k3d, Docker Desktop), off elsewhere ([section 14](#14-security-and-trust-model)) |
| | `debug_capabilities` | auto | `auto` drops SYS_PTRACE / unconfined seccomp with a warning where admission denies them; `require` fails instead |
| | `runtime_class`, `service_account` | the namespace's | a sysbox runtime class runs DinD without privileged pods |
| | `pod_overrides_yaml` | | not applied by this version: a create refuses while it is set |
| | `protect_from_drain` | off | a PodDisruptionBudget per instance (needs the PDB permissions) |
| | `host_gateway` | auto | `off` turns the host-gateway mirror off ([section 8](#8-networking)) |
| quotas | `max_instances`, `max_total_disk` | none | refused with `kubernetes:quota` |
| | `automation_fire_interval`, `automation_max_per_agent` | 30s, 4 | [section 12](#12-automation); `0` turns one off |
| image | `image.mode` | auto | `node-load`, `registry` or `gateway`; auto picks node load on a local cluster, else the registry when set, else the gateway |
| | `image.registry`, `image.insecure_registry`, `image.pull_secrets`, `image.managed_pull_secret` | managed on | registry delivery |
| | `image.builder`, `image.platform` | default builder, the nodes' | the build |
| | `image.retention`, `image.upload_pace_percent` | 72h, 80 | pushed images kept after their last use; gateway upload pacing (0 = off) |
| workspace | `rebuild_export` | auto | `off` rebuilds from the daemon host's checkout as it is |

## 6. Storage

**Instance disks** (`storage_class`, 50Gi). Auto picks a block class,
network-attached before node-local (on one node too: the disk outlives its
node), then with a snapshot class, `WaitForFirstConsumer` binding and volume
expansion, and never a class of static volumes
(`kubernetes.io/no-provisioner`). An NFS class for instance disks warns: root
squash breaks package installs, and Docker inside the instance cannot use
overlay storage on a file share. A node-local class on
a multi-node cluster loses an instance's disk with its node; the connection is
limited until you acknowledge it with `node_local_ack`.

**The fleet share** (`shared_storage_class`, 10Gi) holds a fleet's shared
folders on that cluster: agent logins and settings, `~/.claude.json`, custom
mounts, named volumes. Auto tests classes and picks the mode:

| Mode | When | What it means |
|---|---|---|
| multi-node | a ReadWriteMany class passes the multi-node test (two pods on two nodes, plus a POSIX self-test: rename, symlinks, chmod/chown, uid, unix sockets, O_APPEND, flock) | instances schedule anywhere the share's volume reaches. Codex's SQLite moves pod-local (`CODEX_SQLITE_HOME`); a socket inside a shared folder connects only from the same node; writes made on another node reach watchers a little later |
| single-node | no class passes | all of a fleet's pods run on the node holding its share; the cluster shows `⚠ Each fleet's instances run together on one node (cap N)`, N from node capacity and the volume attach limit. Different fleets can use different nodes |
| copy | one volume cannot be mounted by two pods on one node, or on an SELinux node the second cannot read what the first wrote (the deep test's `share_pods`) | no share: each instance keeps its own copy, and logins may need repeating per instance (limited) |

The first create that makes a fleet share on a connection no deep test has told
runs it first (the create's `testing the cluster` step; concurrent creates
share one run), so the share gets the mode the cluster supports. The multi-node
test takes about a minute per class and is cached; `fleet cluster test
--storage <class>` runs it now. A class with a large minimum size (Filestore)
asks before Auto tests it. The editor says what to expect of common classes:
SMB (Azure Files' built-in classes) keeps no owners or symlinks and EFS access
points give every file one owner, so both fail; CephFS, Longhorn RWX, NFS
without root squash and statically provisioned EFS typically pass.

**Attach limits.** Each instance disk attaches to its node, and CSI drivers
cap attachments per node. In single-node mode a whole fleet's disks and its
share attach to one node; the cap counts them, and a pending pod's row names
the limit.

**Changing the shared class** applies to new fleets at once. An existing fleet
moves once all its instances on that cluster are stopped: copy Jobs move the
share through a temporary `-share-mv` PVC. The editor lists fleets
waiting, moving, failed or blocked. A create, clone or rebuild in a fleet
whose share is moving is refused until the move finishes (a minute or so); a
start calls a pending move off. A move from multi-node to single-node storage
is blocked, and a node-local share cannot move.

**Node drains.** In single-node mode fleet watches the nodes holding shares.
When one is cordoned or tainted for removal, it stops the fleet's instances
there (flushing the sync first), waits for the volumes to detach, and starts
them together on another node (`node cordoned: fleet moving`). With
`protect_from_drain` it pauses 8s so `kubectl drain` sees the budget. This needs
the cluster-scoped node and VolumeAttachment reads; a fleet on node-local
storage cannot move (`node cordoned: fleet can't move (…)`).

**Clone tiers** (`fleet cluster test` names the connection's): snapshot
(VolumeSnapshot of the source disk), csi-clone (a PVC cloned from it), or copy
(a copier Job). See [Clone](#clone).

## 7. Shared folders, logins and sync

On Docker, a fleet's shared folders are host directories bind-mounted into
every instance. On a cluster they live on the fleet share, and fleetd keeps
them converged with the daemon host's `~/.fleet/workspaces/<fleet>`, which
stays the canonical copy. Changes reach the other side within seconds; the
sync pauses while fleetd is down and catches up when it returns.

- **What syncs how.** Login files (`.credentials.json`, `auth.json`,
  `hosts.yml`, `.augment/session.json`) sync live within a location, a logout
  included. `settings.json`, `.claude.json` and Codex's `config.toml` merge key
  by key (an empty `{}` never wins). `history.jsonl` keeps the lines of both
  sides. Transcripts (`.claude/projects/**`), custom mounts and host binds
  mirror; a concurrent change keeps the loser as a conflict copy
  (`fleet sync conflicts`). `downloads/`, `sessions/` (so Codex's session
  transcripts too), `daemon/`, `packages/`, lock files and Codex's SQLite
  databases stay local to each location.
- **Locations and logins.** A location is the daemon host or one cluster, and
  each logs in once per fleet: one login serves every instance of the fleet
  there, and never another location. The agents rotate their refresh tokens,
  so two locations refreshing one login at once could sign each other out.
  Each cluster's logins are kept on the daemon host in
  `~/.fleet/sync/<fleet>/loc/<cluster>/` (0600 files, 0700 folders), so a fleet
  destroyed and recreated on that cluster starts logged in.
- **The Ready gate.** A create or start waits up to 60s for the logins and
  settings to sync into the pod before the lifecycle hooks run, then goes on
  with a warning.
- **Watching it.** An instance row shows a sync badge only when its fleet is
  out of sync (`sync paused: share full`, …), and the fleet options say the same
  under Custom mounts. `fleet sync status [fleet]` shows each folder, what is
  pending and each share; `fleet sync wait [fleet] [--timeout 2m]` waits until
  host and clusters agree (it skips shares with no running pod, and a file
  rewritten faster than the 100ms settle can still be in flight when it
  returns). Nothing is dropped: a folder that cannot sync (a size cap, a full
  share, under 5% free on the daemon host) pauses and says why.
- **Size caps per location:** `.claude` 8 GiB (a warning at 2 GiB of
  transcripts), `.codex` 4 GiB, `.augment` 2 GiB, gh 64 MiB. Custom mounts and
  host binds have no cap.
- **Custom mounts.** A custom mount (`e` on the fleet → Custom mounts, `fleet
  mount add <fleet> <path>`, MCP `fleet_mount_set`) is a folder every instance
  gets. By default it is `~/.fleet/workspaces/<fleet>/.mnt/<path>`; **[f]** in
  the dialog, `--host <folder>` or `host_folder` binds a daemon-host folder of
  your choice as it is (a bigger disk, say). On a cluster either is synced both
  ways. A host folder must exist when an instance is created.
- **Host binds** in devcontainer.json (`~/.aws`, `~/.gitconfig`) sync from the
  real host path: two-way for a read-write bind, host to pod for a read-only
  one (a path any read-write bind names is two-way). A bound file is a folder
  holding that file plus a link, so editors' rename-on-save works. `~/.fleet`
  cannot be bound.
- **Named volumes** live on the share under `.vol/` and are never synced to
  the host. The last instance using one removes it.
- **An injected fsGroup** (an OpenShift SCC, or another mutating admission
  policy) has Kubernetes re-own the share's files to that group as they mount:
  0600 agent logins become 0660, readable by every pod given the group. The
  create warns when admission gives the pod an fsGroup fleet did not set.
- **Destroying a fleet** flushes the sync, then deletes the share
  (`share_retention=delete`). A share whose flush failed is kept and labelled
  `fleet.dev/orphan=flush-failed`; Settings counts it and `fleet cluster gc`
  lists it. A stop whose flush failed is remembered
  (`~/.fleet/sync/<fleet>/unflushed`) until a later flush succeeds; the hub's
  own state lives beside it in `~/.fleet/sync/<fleet>/`.

**Shared state and SELinux.** Every pod of one share uses one SELinux level.
The deep test's `share_pods` runs its two pods at one level too. Where the
second cannot read the first's file (the runtime relabels the class's volumes
per pod, or leaves files at a level the pods lack), the check fails and the
share falls back to copy mode.
An NFS share mounted without `context=` is labelled `nfs_t`, which any pod
allowed `virt_use_nfs` can read: no MCS isolation between fleets on that
server.

## 8. Networking

- **Port forwards** (`p`, `fleet port-forward`) go through the API server's
  port-forward, pooled per pod. Containerd and cri-dockerd do not pass a
  half-close through it; a forward that needs one (an upload that waits for the
  answer after closing its side) takes `--half-close` (`h` in the dialog),
  which uses the agent session instead. Keep it for the forwards that need
  it: the agent session carries the instance's audio and polls too, and over
  a slow link its window caps a stream at about 1.7 MB/s at a 150ms round
  trip, where the pooled port-forward reached 3-12 MB/s.
- **appPort and `-p`** listen on the daemon host. On a cluster they bind
  127.0.0.1 unless the spec names an address (Docker defaults to 0.0.0.0); a
  remote client is warned of one open on the daemon host's network.
- **The browser** (`b`) uses `fleet browser-proxy`, fleet's own HTTP proxy,
  bound to the pod's loopback. It replaces the old privoxy install on every
  instance that can run the daemon's fleet binary; privoxy is kept only where
  it cannot (Docker instances of a macOS daemon, a Coder or Codespaces
  workspace of another architecture than the daemon's).
- **Siblings.** A fleet's pods resolve each other by DNS:
  `<sts>-0.$FLEET_PEER_DOMAIN` (exported by `fleet.rc`). Ingress to an instance
  is allowed only from the same fleet's instances.
- **Cloud metadata** (169.254.0.0/16, fd00:ec2::/64) is denied by NetworkPolicy
  unless `block_metadata=false`; DNS still works. The deep test checks that the
  cluster enforces NetworkPolicy and that pods cannot reach 169.254.169.254
  ([section 14](#pod-hardening)).
- **host.docker.internal.** When the devcontainer declares a host-gateway, the
  pod's `host.docker.internal` is 127.0.0.2, where fleet mirrors the daemon
  host's listening TCP ports (polled every 5s). Unlike Docker, it also reaches
  services bound only to the daemon host's loopback. Fleet's own ports, the
  instance's forwarded and published ports are not mirrored; a pod process
  cannot listen on 0.0.0.0 on a mirrored port; a host-network pod gets no
  mirror. On a macOS or BSD daemon only the daemon user's listeners are seen
  (via `lsof`). `host_gateway=off` turns it off; a remote client is warned
  that the mirror exposes the daemon host's ports to the pods.
- **Host sockets** a devcontainer binds (gpg-agent, X11, Wayland, PulseAudio)
  are bound in the pod by the agent and relayed to the exact host path recorded
  at create. Each is owned by the remote user with the host socket's mode
  (bound as root only in a folder that only root can change); missing parent
  folders are made 0700, owned by the remote user. A bound socket folder
  (`/tmp/.X11-unix`, a gpg agent's) follows the host: fleetd lists it every
  second while a pod that binds it has a session, so a socket made or removed
  there later comes and goes in the pod too (at most 16 folders and 64
  sockets per pod). A socket under `/dev` (`/dev/log`) is relayed like any
  other; binds under `/proc`, `/sys` and `/opt/fleet` are refused.
- **The SSH agent** reaches pods through `/fleet-mounts/control/ssh-agent.sock`,
  relayed like Docker's. `FLEET_SSH_AGENT_SOCK` set to a live socket on the
  daemon host serves pods that agent instead (through the same allowlist);
  set to a missing path it falls back to the relay with a warning in
  `fleet.log`; `off` disables agents in pods, a project's own
  `${localEnv:SSH_AUTH_SOCK}` bind included.
- **In-pod sockets.** `/fleet-mounts/control` is owned by root, 0755; its
  sockets (`fleet.sock`, `ssh-agent.sock`, `mcp.sock`) are 0666 and relayed to
  fleetd only behind the same gates as on Docker. `fleet mcp-bridge` waits up to
  a minute for `mcp.sock` in a pod (`--wait`).

## 9. Docker, Buildkit and the caches

- **Docker-in-Docker** (the docker-in-docker feature, `--privileged`) needs
  privileged pods allowed, or a sysbox runtime class. When the image has a
  dockerd, fleet writes its `daemon.json` before it starts: the pod network's
  MTU (so nested networks never fragment under an overlay CNI),
  `dind_address_pool`, and on a privileged pod a cgroup parent inside the pod,
  so the engine's containers count against it. It records what it set, so a
  setting it stops making (a cleared `dind_address_pool`) goes back to what
  the file had before, unless someone changed it since.
- **Docker outside of Docker** (the docker-outside-of-docker feature, a
  docker.sock bind, `--use-api-socket`): a pod never gets the daemon host's
  engine. It gets its own, the `fleet-docker` sidecar (privileged, or under
  sysbox), so the containers it runs are pod-local siblings: their published
  ports are on the instance's localhost, their binds see the workspace and
  nothing else of the instance, and their images live on the instance disk.
  It needs Kubernetes 1.29+, is refused with `--network host`, and
  `service_images.dind` must be a `docker:<version>-dind` image. `fleet logs`
  shows the dev container only; the sidecar's log is `kubectl logs <pod> -c
  fleet-docker`. The sidecar requests 50m CPU and 128Mi and takes the dev
  container's limits (the containers it runs share its cgroup), so a
  ResourceQuota admits it without a LimitRange.
- **Shared Buildkit** follows the fleet's own toggle, where the connection's
  `allow_shared_buildkit` permits it. The server runs rootless unless
  privileged pods are allowed, and is reached over mutual TLS: each instance
  gets its own client certificate in `~/.fleet/buildkit`, valid 7 days, renewed
  daily while fleetd runs. A certificate within 48h of expiry puts `Buildkit
  credential expires in Xh; it renews while the Fleet daemon runs` on the
  instance; past expiry its builds fail until fleetd renews it. Each fleet's CA
  is rotated when the fleet leaves the connection, and its private key is in
  the hourly backup (`kube/<cluster>/buildkit/<fleet>.json`, a secret).
  buildkitd does not log client certificates; fleetd logs each one it issues.
  Its GC keeps the cache under 80% of its volume. Its cache volume belongs to
  one shape: when `allow_privileged` flips, the server of the other shape
  starts on an empty cache.
- **The deb and image caches** run in the namespace as per-fleet Deployments
  on their own volumes, kept when the fleet is destroyed (unless
  `delete_cache_volumes_on_destroy`) and deleted with the connection.
  Instances reach them by short Service name. apt uses them through a
  Proxy-Auto-Detect script that falls back to DIRECT while the cache is down
  (the probe keeps trying for 8s first, so a cache that is really down costs
  that once per host per apt run). A DooD engine gets the image mirror through
  `FLEET_DOCKERD_CONFIG`, and fleet merges it into a DinD engine's
  `daemon.json`. The caches are
  unauthenticated: NetworkPolicy labels keep other pods out, and anyone who can
  label a pod in the namespace gets in.

## 10. Audio

The microphone and audio output work on clusters. They ride the pod's agent
session, and while audio flows the bulk session (file copies, sync) is paced so
frames arrive on time (`fleet.log`: `kube agent: bulk session paced while audio
flows`).

On a cluster **without root**, nothing in the pod can install the audio stack.
fleet adds it as a root-built layer to the images it builds there; a prebuilt
image (`image`, no Dockerfile, no features) is never built, so it has audio
only if it ships PulseAudio and ALSA. The Deploy row warns before the create;
after it, the instance's banner says `audio unavailable on cluster <name>:
containers can't run as root. Use an image with PulseAudio and ALSA installed.`
and Settings → Microphone counts such instances.

## 11. Lifecycle

**Stop and start.** Stop flushes the sync and scales the StatefulSet to 0;
the disk keeps everything. Start brings the pod back, re-delivers the image if
the nodes lost it, waits for the shared state, and runs `postStartCommand` and
`postAttachCommand`.

**Restarts the cluster makes.** A pod the cluster restarts or moves (a drain,
an eviction, a crash, `kubectl delete pod`) comes back on its own, and its
start hooks run again. Its row says `sessions reset: <cause>` until your next
attach or until fleet stops, starts or rebuilds it, with the setting that
prevents it when there is one. Restarts while
fleetd was down are not reported. Three crashes in 5 minutes, or the kubelet's
CrashLoopBackOff, park the instance as stopped (`kubernetes:crash_loop`, or
`kubernetes:oomkilled` naming the memory limit).

**Out of memory.** On Docker an OOM kill takes one process. On a cluster with
cgroup v2 (Kubernetes 1.28+) on containerd or CRI-O, the kubelet makes it take
the whole container (cri-dockerd still kills one process), so every session
ends and the row says `restarted: out of memory, sessions ended`. fleet
restores the Docker behaviour where it can: on a privileged pod it sets
`memory.oom.group` to 0 and gives the user's processes a `memory.max` just
under the container's limit, so the kernel kills one process. An unprivileged
pod keeps the whole-container kill unless the cluster's owner sets the
kubelet's `singleProcessOOMKill: true` (Kubernetes 1.32+). A pod over its
memory request is evicted first under node memory pressure: raise
`resources.memory_request` (hinted on the row).

**Rebuild** (`R`, `fleet rebuild`) keeps the workspace and replaces the root:

1. Export the workspace to the daemon host (from the running pod, or through a
   tool Job, `<sts>-xprt-g<n>`, when stopped). `rebuild_export=off` skips it
   and builds from the host's checkout as it is.
2. Build and deliver the image, check the disk has room (growing it when
   `auto_expand_disk` and the class allow), and have admission judge the new
   pod.
3. Scale down, move the old root to the disk's trash, seed generation N+1, and
   start it (the ContainerID ends `@g<n>`).
4. On failure, roll back (`<sts>-rlbk-g<n>`). The record fails
   `kubernetes:rebuild_failed` in one of three forms: `the rebuild failed;
   generation N was restored: …`, `the rebuild failed before it changed the
   instance: …` (the old pod kept running throughout), or `undoing …` when the
   rollback itself failed, which the next fleetd start finishes. After a
   success the trash is deleted in the background (`<sts>-cmit-g<n>`).

A failed create retries the same way. Rebuilding a failed clone destination
does a fresh checkout. A rebuild the daemon stopped before it changed the
instance leaves it as it was; one stopped later is rolled back by the next
fleetd. An instance whose StatefulSet was deleted outside fleet (its disk
kept) is rebuilt as a stopped one, over that disk. Each run of the lifecycle
hooks (create, rebuild, start) has a ceiling, `FLEET_KUBE_SETUP_TIMEOUT` (1h),
past which it fails `kubernetes:timeout`, naming what still runs in the pod.

<a id="clone"></a>**Clone** (`C`, `fleet clone`) copies the source's disk:
snapshot first, then a CSI clone, then a copier Job, whichever the connection
supports (`clone_method` forces one). On a privileged source pod, the source's
processes, Docker-in-Docker's containers included, are frozen while the
snapshot is cut or the copier takes its final pass; a watchdog thaws them 30s
after fleetd's last renewal and after 2 minutes at most. Without the freeze a
snapshot is crash-consistent, and a copy of a running source is best-effort (a
live pass, then a final delta; the clone is marked so). Git refs are copied
before objects, so a copied ref never names a missing object.
`clone_consistency=stop-source` stops the source for the clone instead. A
second clone of one source waits for the first. Very large clones on
csi-hostpath fall back to the copier. A clone keeps the source's root and runs
no lifecycle hooks.

**Destroy** deletes everything the instance made, by its key, even when the
create never finished. `fleet cluster gc <c>` lists objects labelled for this
daemon that no record accounts for; `--apply` deletes them while this daemon
holds the connection's owner Lease. It keeps mounted volumes, objects younger
than an hour and those of existing fleets (`--fleet` names one to delete too),
and a stale job journal is not garbage. Exact known garbage (a failed job's
objects) is deleted automatically.

**Ownership.** Each daemon has an id (`~/.fleet/daemon_id`) and holds a Lease
per connection. A restored backup or a copied `~/.fleet` on another machine
sees `owner conflict` until the first daemon's Lease expires.

**Shutdown.** Stopping fleetd ends its streams within a bounded budget; a create
cut short is left for the next start to fail as interrupted.

## 12. Automation

Agents can run on a cluster: pick it as the agent's target (TUI agent dialog,
`fleet agent create --cluster`, MCP `cluster`). Without one an agent runs on
`devcontainer`, never on the remembered default, since it fires unattended.

- **Limits per connection:** a trigger fires at most once per
  `automation_fire_interval` (30s) and one agent holds at most
  `automation_max_per_agent` (4) runs there. A run over a limit waits in its
  trigger's queue (20 firings, each kept an hour); room that frees up goes to
  the run waiting longest. `fleet agent list` (its STATUS column) and
  `fleet_automation_list` (`queued`, `running`, `login`) say what each agent
  waits on. A webhook delivery answers
  200 when everything fired, 202 when some of it queued (or a full queue dropped
  a trigger), 429 with `Retry-After` when every matching queue is full, and 503
  when the scheduler is busy (nothing fired). `max_instances` caps a runaway
  webhook.
- **Outcomes.** Every firing's log ends with `# outcome:` lines: the instance
  it got, or why not (queued, refused, dropped, not created, failed, reaped),
  in `L`, `fleet trigger logs` and `fleet_trigger_logs`.
- **Logins.** A cluster agent whose tool is not logged in there (and has no API
  key variable in its command, or one with a value in the devcontainer's env,
  `${localEnv:…}` resolved in fleetd's environment) is refused, with `⚠ log
  in to Claude once for this fleet on ☸ <cluster> (L)` on its row; `L` on the
  agent logs its tool in where it runs. `FLEET_AUTOMATION_LOGIN_CHECK=off` turns
  the check off. An agent on a connection that was removed shows
  `☸ <id> (missing)`.
- **Runs.** A cluster run's create has a deadline
  (`FLEET_AUTOMATION_CREATE_TIMEOUT`, 20m). A failed run releases its pod and
  disk; an interrupted one is destroyed.
- **Seed cache.** The first run of a fleet build on a connection whose disks
  can be cloned keeps a trimmed copy of its seeded disk; later runs start from a
  clone of it. A cache unused for 7 days is deleted, as are all of them when the
  connection is removed; `fleet cluster gc` leaves them alone.
  `FLEET_KUBE_SEED_CACHE=off` seeds every run in full.

## 13. VS Code

`c` (or `fleet code`) on a cluster instance, or on a Docker instance of a
remote daemon, opens VS Code through **Remote-SSH** (install the Remote-SSH
extension). There is no SSH server listening anywhere: fleet writes an alias
for the instance into `~/.ssh/fleet_config`, and the first time asks to add one
`Include` line to your `~/.ssh/config` (`fleet code --print` prints the command
and never edits it). `ssh <alias>` runs `fleet ssh-proxy`, which reaches the
instance through the daemon's authenticated Exec and starts `fleet ssh-server
--stdio` there: no listener, no SSH authentication of its own. For a
`FLEET_GATEWAY` / `FLEET_SSH` / `FLEET_SERVER` remote, the route (and a
`FLEET_TOKEN`) is kept on your local daemon in `~/.fleet/editor_endpoints.json`
(0600, never sent to the remote). To a remote fleet with `[ agent: on ]`,
`fleet ssh-proxy` carries your ssh-agent for the editor's connection, as
`fleet shell` does.

`customizations.vscode` extensions and settings apply once VS Code's server
runs in the instance; `postAttachCommand` runs once per VS Code connection. For
an air-gapped cluster set `"remote.SSH.localServerDownload": "always"`. A
custom `remote.SSH.serverInstallPath` is not watched, so customizations are not
applied there.

## 14. Security and trust model

**The namespace is the trust boundary**, or the node once privileged pods are
allowed. fleetd treats pods as untrusted: their streams are capped, host writes
go only into sync roots through a symlink-safe writer, and host dials go only
to recorded sockets and mirrored ports. fleetd itself acts on a pod with the
authority of `docker exec`: the agent runs as root in the pod (as the one
non-root user on a cluster without root).

Anyone who can write to the namespace (`pods/exec`, create pods, patch
StatefulSets) is effectively inside its instances. Anyone who can mount or
snapshot a fleet's share has its agent logins. Settings → Kubernetes says so,
and when RoleBindings are readable, `fleet cluster test` names the other
subjects that hold those verbs. Pod verification defeats a substituted pod; it
does not defend against someone who can exec into the real one.

### What enters the namespace

| Item | In the namespace? |
|---|---|
| kubeconfig, cluster tokens, MCP bearer token, gateway token | never |
| agent logins (`.credentials.json`, `auth.json`, `hosts.yml`, `.augment/session.json`) | yes, on the fleet share (the instances need them) |
| `containerEnv` / `remoteEnv` values | in the instance's `<sts>-env-g<n>` Secret |
| Buildkit client key | per instance, valid 7 days, renewed daily |
| built image layers | in each node's image store after a pull; with registry delivery also in the registry; with gateway delivery no registry holds them: fleetd serves them and the gateway relays them (TLS ends at the gateway, which sees them in transit) |
| gateway pull credential | in the managed pull Secret: anyone who can read Secrets in the namespace can pull this cluster's images through the gateway from anywhere, until the connection is removed |
| gateway registry id | in image references, readable by anyone who can read pods; it is not the session id that webhook URLs use as their credential |

Annotations hold hashes only, the build capture is redacted, `plugin_env`
takes no value for a name that looks secret, and creation logs redact `-e`
values. **Secrets at rest are the cluster operator's concern**: Kubernetes
stores Secrets unencrypted in etcd unless encryption at rest is configured, so
enable it (or use a KMS provider) on clusters that hold agent logins and env
values.

### Privileged pods

`allow_privileged` is on only for detected local clusters and off elsewhere
until you turn it on. A privileged pod can reach every pod on its node, so the
node becomes the isolation boundary. Before turning it on:

- run fleet's pods on a **dedicated node pool** (`node_selector`,
  `tolerations`) that runs nothing else;
- prefer a **sysbox runtime class** where the cluster offers one: DinD and the
  DooD sidecar run under it without privilege on the node;
- remember what it unlocks: DinD, the DooD sidecar, rootful Buildkit,
  `--device`, host namespaces.

Privileges are never added or dropped silently: what needs privilege fails
with the setting to change, and debug capabilities are dropped only with a
warning (`debug_capabilities=auto`).

### Pod hardening

Every pod fleet makes (instances, their Jobs, the caches, the deep test's)
gets no ServiceAccount token, no service links, seccomp `RuntimeDefault`, a
service-mesh injection opt-out, `OnRootMismatch` volume ownership and one
SELinux level per share group, and no host namespaces or hostPath unless it is
a privileged instance that asked. The browser proxy and landing page listen on
the pod's loopback, and the control directory is root-owned. NetworkPolicies
let an instance take connections only from its own fleet and deny egress to
cloud metadata.

Policies are only as good as the network plugin. The deep test says
`NetworkPolicies are not enforced here: a fleet's pod isolation and metadata
block are advisory` when they are not, and `169.254.169.254 reachable from
pods (egress block on)` when the block does not hold: use a plugin that
enforces them (Calico, Cilium), and the cloud's own metadata hardening (an
IMDSv2 hop limit of 1, GKE Workload Identity), since a node identity reachable
from a pod is a credential for the whole node.

### Remote access

A remote TUI or CLI (Remote Fleet through the gateway with the bearer token,
or Armada SSH with an SSH login to the daemon host) is as powerful as a local
one, on purpose. It can set `extra_path` and `plugin_env`, which decide what
fleetd runs on the daemon host, and `runtime_class`, `service_account` and
`allow_privileged`, which reach the node. A leaked token or SSH login is
therefore as good as a shell on the daemon host, and on any node that runs
privileged pods. Two RPCs stay on the local socket because they serve the
`fleet kube-exec` shim on the daemon host (`PodExec`,
`IssueKubeExecCredential`); remote clients run the same commands through the
streaming Exec RPC, and never receive kubeconfig contents or tokens. Plugin
stderr reaches them sanitized and cut to 512 bytes.

An instance of a fleet with **Fleet MCP** on can drive every connected cluster
through it, like any other fleet tool.

### Files from instances

A rebuild's workspace export is unpacked on the daemon host as the daemon's
user, keeping setuid and setgid bits: a user in the pod can leave a setuid
binary there owned by the daemon user, as a Docker bind mount of the workspace
already allows.

## 15. Environment variables

Read by fleetd (set them where the daemon starts). Durations are Go durations
(`90s`, `20m`); the `FLEET_KUBE_*_TIMEOUT` ones also take whole seconds.

| Variable | Default | What it does |
|---|---|---|
| `FLEET_KUBE_PVC_BIND_TIMEOUT` | 5m | a disk that has not bound fails `kubernetes:pvc_pending` |
| `FLEET_KUBE_UNSCHEDULABLE_TIMEOUT` | 3m | a pod the scheduler refuses fails `kubernetes:unschedulable` |
| `FLEET_KUBE_IMAGE_PULL_TIMEOUT` | 15m | an image the nodes cannot pull fails `kubernetes:image_pull` |
| `FLEET_KUBE_SEED_TIMEOUT` | sized to the image | the seed Job |
| `FLEET_KUBE_POD_READY_TIMEOUT` | 10m | the pod's own start |
| `FLEET_KUBE_AGENT_TIMEOUT` | 2m | the agent's bootstrap |
| `FLEET_KUBE_SHARED_STATE_TIMEOUT` | 60s | the Ready gate |
| `FLEET_KUBE_SNAPSHOT_TIMEOUT` | 30m | a clone's snapshot |
| `FLEET_KUBE_COPY_TIMEOUT` | 2h | a clone's copier Job |
| `FLEET_KUBE_SETUP_TIMEOUT` | 1h | one run of the lifecycle hooks (create, rebuild, start) |
| `FLEET_KUBE_SEED_CACHE` | | `off` seeds every automation run in full |
| `FLEET_AUTOMATION_CREATE_TIMEOUT` | 20m | a cluster automation run's create |
| `FLEET_AUTOMATION_LOGIN_CHECK` | | `off` skips the cluster agent login check |
| `FLEET_EXEC_HOLD_GRACE` | 12h | how long fleetd waits for a dropped agent session to come back before it fails the commands that session held; the pod agent keeps them running as long as the same variable in the pod's environment says (12h when unset) |
| `FLEET_SSH_AGENT_SOCK` | | see [section 8](#8-networking) |
| `FLEET_POD_BINARY_<ARCH>`, `FLEET_POD_TMUX_<ARCH>` | `~/.fleet/bin`, else the release download | the pod build and tmux bundle fleetd stages into pods ([section 2](#2-what-you-need)) |

In instances: `FLEET_PEER_DOMAIN` (sibling DNS) and, with DooD,
`FLEET_DOCKERD_CONFIG` (the sidecar engine's `daemon.json`).

## 16. Known differences from Docker

- **Not supported:** compose devcontainers; `--network <custom>`, `--link`,
  `--volumes-from`; device and FIFO binds (`kubernetes:unsupported:*`).
- **Accepted, with a warning:** `--hostname` (the pod's hostname is `<sts>-0`
  unless SYS_ADMIN is admitted), `--pids-limit` and `--memory-swap` (no per-pod
  knob), `--ulimit` hard raises (need CAP_SYS_RESOURCE).
- An OOM kill on an unprivileged pod restarts the instance ([section
  11](#11-lifecycle)).
- Each top-level folder of the root (`/etc`, `/usr`, `/home`, …) is its own
  mount of the instance disk, so `rename(2)` and hard links between them fail
  with EXDEV: Python's `os.rename`/`os.replace`, Perl's `rename`, Go's
  `os.Rename` and `ln`/`cp -al` across them break, while `mv` (it copies), apt,
  dpkg, pip, npm, go and git work; uv falls back to copying without a word.
  GNU `df` lists the disk once; `df /` shows the pod's ephemeral overlay, not
  the disk.
- Shared folders are eventually consistent, within seconds, and pause while
  fleetd is down ([section 7](#7-shared-folders-logins-and-sync)).
- A host-gateway mirror reaches loopback-only host services.
- On a privileged pod, everything a user runs is in the pod's `fleet-user`
  cgroup, under its `memory.max`. On containerd and CRI-O, the
  docker-in-docker feature's entrypoint still creates an empty `init` cgroup at
  the node's cgroup root (its containers no longer land there).
- `fleet logs` shows the dev container only, not a DooD sidecar.
- `fleet exec` output rides the pod's agent session, so over a slow link it
  is window-bound (about 1 MB/s at a 150ms round trip, where `kubectl exec`
  reaches about 20); `fleet copy`, folder copies and the shared-folder sync
  use the bulk session (a 48MB copy took about 5s each way there).
