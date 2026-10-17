---
name: fleet-admiral
description: >-
  Teaches you what fleet is and how to drive its MCP tools (fleet_list,
  fleet_up, fleet_exec, fleet_session_spawn, ...). Use this when you need to
  interact with fleet in any way, especially when kicking off coding agents
  in fleet instances.
---

# Fleet Admiral

`fleet` manages a fleet of isolated dev instances — each a devcontainer with its
own checkout of a repository — and can run a coding agent inside any of them.
fleet exposes its capabilities as MCP tools (the `fleet_*` tools from the
`fleet` MCP server). This skill gives you an understanding of what fleet is and
how to drive those tools, so you can put them to use however a task calls for.
It does not prescribe a workflow: you decide how to apply these capabilities to
what the user asks.

## Concepts

```
   fleet  (a repository)
     ├── instance A   ← isolated devcontainer, its own checkout
     │     └── agent  ← a coding agent can run inside (driven via a session)
     ├── instance B
     └── instance C
```

- **Fleet** — a group of instances tied to one repository. You name it when
  you create its first instance (conventionally the repo basename, but any
  name works); use `fleet_list`/`fleet_status` to find existing names rather
  than deriving them from URLs.
- **Instance** — one devcontainer: an isolated workspace you can run
  commands in, open in an editor, or run an agent inside. It runs where its
  **backend** puts it: `devcontainer` (Docker on the daemon host), `coder`,
  `codespaces`, or `kubernetes` (a pod on one of the daemon's cluster
  connections; `fleet_list` shows the backend). It has a lifecycle
  (`creating → running ⇄ stopped → deleting`, plus `failed`; transitional
  `cloning`, `starting`, and `stopping` can also appear in `fleet_list`).
- **Session** — a named, *detached* tmux session inside an instance. Sessions are
  how you interact non-interactively with something long-lived in an instance
  (most importantly, a coding agent): you create the session, send keystrokes to
  it, and read back its current screen. It persists across separate tool calls
  until you stop the instance or kill it.

## Addressing instances

Tools that target an instance take explicit `fleet` and `instance` string
parameters — there is no working-directory inference and no combined
`fleet/instance` form. (Fleet-level tools take just `fleet`; `fleet_clone`
names its instances `source` and `destination`; `fleet_copy` takes a
`{fleet, instance, path}` object per side.) Use `fleet_list` or
`fleet_status` to discover the names when you don't know them.

## What the tools can do

All tools return structured JSON — no text output to parse.

**Inspect state.** `fleet_status` summarizes every fleet (instance counts by
running/stopped/other, plus the repo remote). `fleet_list` returns per-instance
records (fleet, instance, status, branch, current_branch, tag, backend,
container_id, workspace_dir, created_at, error), optionally filtered to one
fleet. `branch` is the branch the instance was created from; `current_branch`
is the one its workspace has checked out now. `fleet_version` reports
the daemon's version and liveness. `fleet_logs` returns an instance's container
logs (use `tail` to cap to the last N lines).

**Recover daemon state.** fleetd snapshots its own state (config, registry, MCP
files, Kubernetes connections) to `~/.fleet/backup/<date>/<hour>.tar.xz` every
hour; instance disks on Kubernetes stay in their clusters. `fleet_restore_backup`
is documentation-only — it restores nothing — but tells you where those archives
live, which ones exist, what each contains, and the manual procedure to restore
one (the daemon must be stopped first, then the archive is unpacked over
`~/.fleet`). Reach for it when asked to roll fleet state back to an earlier point.

**Manage instance lifecycle.** The slow, job-shaped tools — `fleet_up`,
`fleet_clone`, `fleet_rebuild`, `fleet_down`, `fleet_destroy_fleet` — are
async-first: they return within seconds with `{job_id, done: false, instance}`
while the job keeps running server-side. Kick off as many as you need (e.g. bulk-provision
several instances), keep working, then poll `fleet_job_status {job_id}` until
`state` leaves `running` — it reports `succeeded` (with `warnings` and the
final `result` record) or `failed` (with `error`). The instance's `status` in
`fleet_list` tells the same story (`creating → running`, or `failed` with
`error`; teardown shows `deleting` until the record disappears). Pass
`wait: true` to block until the job finishes instead — `fleet_up` provisions a
devcontainer (expect minutes, not seconds), so only do that when your
tool-call timeout allows it. A blocking call returns
`{job_id, done: true, success, instance, warnings}`, and a failed blocking job
surfaces as a tool error.

- `fleet_up {fleet, instance, remote?, branch?, backend?, cluster?, settings?, wait?}` creates and
  starts an instance. `remote` is required only when creating the first
  instance of a new fleet; after that the fleet's recorded remote is used. It
  is normally a git URL, but `file:///abs/dir` names a local TEMPLATE directory
  on the daemon host instead: fleet copies it into each new instance rather
  than cloning (a template fleet takes no `branch` and only the `devcontainer`
  or `kubernetes` backend). `settings` gives the new fleet its options (the
  object `fleet_edit` takes, see Fleet options) before its first instance is
  made; it is refused for a fleet that already exists.
  `backend` selects where it runs: `devcontainer`, `coder`, `codespaces`, or
  `kubernetes` (a pod on one of the daemon's cluster connections).
  `cluster` names that connection by id, name or kube context (see
  `fleet_cluster_list`) and implies `kubernetes`; with `backend: kubernetes`
  and no `cluster`, the remembered or only connection is used.
  Omitted, the daemon uses its remembered default deploy target (the one last
  picked in a TUI New instance dialog, which may be a cluster; `devcontainer`
  out of the box), and the reply's `target` names the backend and cluster it
  used. So an unspecified create goes wherever the user last chose, not
  necessarily to this machine: pass `backend` or `cluster` when where it runs
  matters (the user named a place, or the work needs a specific one). Coder,
  Codespaces and clusters need prior configuration, so only name one the user
  has set up (`fleet_cluster_list` shows the clusters).
- `fleet_start` / `fleet_stop {fleet, instance}` resume / pause an instance.
  Stop preserves the workspace; start brings it back. These always block —
  they finish in seconds.
- `fleet_down {fleet, instance, wait?}` stops and removes a single instance.
  `fleet_destroy_fleet {fleet, wait?}` removes a whole fleet and all its
  instances. **Both are irreversible** and discard any uncommitted work in
  those instances.
- `fleet_clone {fleet, source, destination, ..., wait?}` duplicates an
  instance, keeping its installed state (optional `display_name`, `tag`,
  `color`, `branch` overrides).
- `fleet_rebuild {fleet, instance, wait?}` recreates an instance's container in
  place (e.g. after its devcontainer config changed), **preserving the
  workspace** — the git checkout and uncommitted edits survive. The
  `devcontainer`, `codespaces` and `kubernetes` backends support it; `coder`
  does not. It is also the **retry** of a failed `kubernetes` instance: one
  whose create was interrupted or failed (what it left is deleted and the
  create runs again), or whose rebuild failed and was rolled back.
  A failed cluster instance's `error` starts with a `kubernetes:` code
  (`kubernetes:quota`, `kubernetes:image_pull`, `kubernetes:oomkilled`, …)
  that says what went wrong and usually which setting fixes it.
- `fleet_job_status {job_id}` reports a lifecycle job:
  `{state: running|succeeded|failed, step?, error?, warnings?, result?}`; while
  a create, clone or rebuild runs, `step` names where it is (`cloning
  repository`, `starting container`, …). A failed job
  is data here (`state: "failed"`), not a tool error. An unknown `job_id`
  (daemon restarted, or the result expired) is a tool error — fall back to the
  instance's `status`/`error` in `fleet_list`.

**Run a one-off command.** `fleet_exec {fleet, instance, command}` runs a
command inside an instance and returns `{stdout, stderr, exit_code}`. `command`
is an argv **array**, not a shell string — `["git", "log", "--oneline"]` — so
there is no quoting or flag-parsing to worry about. A non-zero exit comes back
as data, not a tool error: check `exit_code`. Not for long-running or
interactive commands — use a session for those.

**Move files.** `fleet_copy {src, dst}` copies a file or a directory between
the daemon host and an instance, or between two instances — the MCP form of
`fleet copy`. Each side is `{fleet, instance, path}` for a path in a running
instance (relative = its workspace folder), or just `{path}` for a path on the
daemon host: the machine fleetd runs on, which is not the instance you may be
running in. A host path is absolute or starts with `~/`. The daemon moves the
bytes itself, so they never pass through your context and size does not
matter; the call blocks until the copy is done and returns
`{path, bytes}` — where it landed. A destination that is an existing
directory, or ends in `/`, keeps the source name inside it; any other path is
the copy's own name, replacing a file already there. A directory copies
recursively and merges into an existing one, skipping symlinks and special
files (not supported on `codespaces`).

**Drive an agent (or any long-lived process) via sessions.** This is the
non-interactive loop for working with a coding agent inside an instance:

- `fleet_session_spawn {fleet, instance, session}` — create a detached tmux
  session.
- `fleet_session_exec {fleet, instance, session, command}` — type a command
  (one shell string) into the session + Enter. Fire-and-forget: it sends
  keystrokes and returns immediately, without waiting for the command.
- `fleet_session_read {fleet, instance, session, scrollback?}` — capture the
  session's screen. `scrollback`: `0` (default) = just the visible screen,
  positive N = the last N history lines, negative = full history.
- `fleet_session_list {fleet, instance}` — the instance's tmux sessions
  (session, windows, created_at, attached).

Session names are canonicalized to `<instance>~<name>` internally so the
session appears as a regular session group in the fleet TUI — keep using the
short name with these tools (they all resolve it the same way);
`fleet_session_list` reports the canonical names.

To put an agent to work, spawn a session, exec the agent's launch command with
the task as its prompt, then read the session to see what it's doing or asking.

**Kubernetes clusters.** The daemon can hold connections to Kubernetes
clusters (a kubeconfig context and namespace each, on the daemon host);
`fleet_cluster_list {cluster?}` returns each with its health — `state`
(`ready`, `limited`, `unreachable`, `unauthorized`, …), `usable` (ready or
limited: creates can go there), `message`, `warnings` — plus its settings and
the capabilities its last check found. `fleet_cluster_test {cluster, deep?,
storage_classes?, test_storage?}` runs the checks now and returns each with a
fix line; `deep` adds throwaway-pod probes and takes minutes. A `cluster`
argument anywhere takes the connection's id, name or kube context. No tool
adds, edits or removes a connection: the user does that (TUI Settings →
Kubernetes, `fleet cluster`). On a cluster an instance behaves like any other,
with a few differences worth knowing:

- a create takes longer (the image is built on the daemon host, then delivered
  to the cluster), and a compose devcontainer is refused there;
- the fleet's shared folders (agent logins, `~/.claude.json`, custom mounts)
  live on a per-fleet volume in the cluster kept in sync with the daemon host
  within seconds, so a file written on one side shows on the other a moment
  later, not at once;
- each cluster logs the agents in once per fleet, so the first agent of a
  fleet on a cluster may ask to log in;
- an out-of-memory kill can restart the whole instance and end its sessions.

**Custom mounts.** A fleet's custom mounts are folders every instance of the
fleet gets at a path of the fleet's choosing. `fleet_mount_list {fleet}` lists
them; `fleet_mount_set {fleet, path, host_folder?}` adds one, or re-points it:
without `host_folder` it is the fleet's own `.mnt` folder on the daemon host,
with one it binds that absolute daemon-host folder as it is (synced both ways
on a cluster), which must exist. `fleet_mount_delete {fleet, path}` removes
one and leaves its content alone. Instances take a change at their next
create or rebuild, and a set returns any warning (a host folder missing).

**Fleet options.** A fleet's options are the settings of its Edit fleet
dialog in the TUI, shared by all its instances. `fleet_settings {fleet}`
returns them; `fleet_edit {fleet, settings}` changes the ones you pass and
returns the result; `fleet_up` can set them on a fleet it makes. They are:

- `claude_code_mount`, `codex_mount`, `auggie_mount`, `gh_mount` — one shared
  login and state for Claude Code, Codex, Auggie or the GitHub CLI across the
  fleet's instances, so it is logged in once for all of them. They land under
  `home_dir`, the home of the instances' user: empty means `/home/vscode`,
  which suits the stock devcontainer images; set it when the image's user is
  another (`/home/node`, `/root`), or the logins land where nothing reads them.
- `fleet_mcp` — serves these same tools and this skill to the coding agents in
  the fleet's instances: the full set, every fleet, which gives anything
  running in them host-level access (a risk the user takes on by turning it
  on).
- `prefer_fleet_launch` — which page the built-in browser opens when the
  devcontainer.json configures both an `initialUrl` and a Fleet Launch page.
- `layout_presets` — `[{name, pane_commands, layout?}]`, the pane layouts the
  TUI offers when a session is made: one startup command per pane (`""` is a
  plain shell). `layout` is a captured tmux geometry; omitted, a same-named
  preset keeps its own while its pane count is unchanged, else the panes stack.
- `custom_mounts` — `[{path, host_folder?}]`, as `fleet_mount_set` takes them.
- `buildkit_server`, `deb_cache_server`, `image_cache_server` — shared caches
  for the fleet's docker builds, apt packages and Docker Hub pulls.
- `coder_workspace_name`, `coder_template`, `coder_preset`,
  `coder_parameters` (`[{name, value}]`) — how the `coder` backend creates the
  fleet's workspaces. Clearing the template clears the preset and parameters.

A list you pass replaces the whole list (`[]` empties it), so read the fleet's
options first to change one entry. The logins, home dir, mounts and caches
reach an instance when it is created or rebuilt.

**Configure automations.** A fleet can run unattended: **agents** are worker
definitions (a launch command with `${PROMPT}`/`${SYS_PROMPT}` placeholders, a
system prompt, an env backend) and **triggers** fire one or more agents — on a
cron **schedule**, a gateway **webhook** event, or a cron-polled **bash** command
that exits zero — each firing spins up a fresh instance that runs the agent.
These tools let you set that up for the user.

- `fleet_automation_list {fleet}` returns the fleet's `{agents, triggers}`;
  each agent also says what it waits on: `queued` and `running` runs, and
  `login`, why the login check refuses its firings on a cluster. Read it first
  before an update — updates are field-merges, so you send only what changes.
- `fleet_agent_create {fleet, name, command?, system_prompt?, backend?, cluster?}` and
  `fleet_agent_update {fleet, name, new_name?, command?, system_prompt?,
  backend?, cluster?}` (omit a field to keep it; `new_name` renames and rewrites the
  triggers that reference it). `cluster` (a connection's id, name or kube
  context) runs the agent's instances on that Kubernetes connection; with
  neither `backend` nor `cluster` an agent runs on `devcontainer`, never on the
  remembered default, since it fires unattended. `fleet_agent_delete {fleet, name}` refuses while
  a trigger still references the agent — detach it first.
- `fleet_trigger_create {fleet, name, type, agents[], prompt?, ...}` where
  `type` is `schedule` (needs `cron`), `bash` (needs `cron` + `script`, a command
  run on the fleet host whose zero exit fires the agents and whose stdout is the
  payload), or `webhook` (needs `webhook_name` + `filter_type` `regex`/`jsonpath`
  and the matching `regex` or `json_path`+`json_value`); `agents` names the agents
  it fires (each must exist). `fleet_trigger_update` merges fields like the agent
  one; `fleet_trigger_delete {fleet, name}` removes it.
- `fleet_trigger_logs {fleet, trigger}` returns `{logs, count}` — the recorded
  payloads of the trigger's recent firings (webhook body, bash command stdout, or
  schedule fire-time), concatenated oldest-first, each followed by `# outcome:`
  lines saying what became of it for each agent: the instance it got, or why
  not (queued, refused, dropped, not created, failed, reaped). Use it to debug
  what fired (or failed to fire) an automation; the daemon keeps the last 100
  firings per trigger. On a cluster, the connection limits how often a trigger
  fires and how many runs one agent holds there, so a firing can wait in a
  queue, and an agent whose tool is not logged in on that cluster is refused;
  the outcome lines say so.

Every write returns the fleet's resulting `{agents, triggers}`. Names are
unique per fleet; an invalid cron, an unknown agent reference, or a duplicate
name comes back as a tool error.

## Behaviors worth knowing

- An instance must be `running` to exec into it or use its sessions;
  `fleet_start` a `stopped` one first.
- Sessions are detached and survive between tool calls; reading one is a
  snapshot of its current screen, not a stream.
- Errors come back as tool errors with a clear message (instance not found, not
  running, blocking job failed); the exceptions are `fleet_exec`, where a
  non-zero exit is ordinary result data, and `fleet_job_status`, where a failed
  job is ordinary result data (`state: "failed"`).
- Lifecycle `warnings` are non-fatal issues from an otherwise-successful job —
  surface them, don't treat them as failure.
- When awaiting an async job, poll `fleet_job_status` every few seconds (a
  `fleet_up` typically takes 1–3 minutes) rather than spinning; you can do
  other work between polls.
- These tools may also be reaching you INSIDE a fleet instance: a fleet with
  its "Fleet MCP" setting on serves them (and this skill) to the agents in its
  instances, and the server's instructions name the instance you are in. They
  are the same full set, for every fleet. Instances you create outlive you —
  clean them up with `fleet_down` — and never stop, rebuild or remove the
  instance you are running in.
- If the `fleet_*` MCP tools are not available, the `fleet` CLI offers the same
  operations from the shell (`fleet --help`). The MCP server is registered in
  Claude Code automatically when the fleet TUI runs; a missing registration
  usually means fleet hasn't been started yet, the current Claude Code session
  predates the registration (the config is read at session start — a new
  session picks it up), or fleet points at a remote daemon via
  `FLEET_GATEWAY`/`FLEET_SERVER`, where registration is intentionally skipped
  and the MCP config must be copied from the TUI settings page.

## Tool reference

```
INSPECT
  fleet_status                                      # per-fleet + total counts
  fleet_list {fleet?}                               # instance records
  fleet_version                                     # daemon version/liveness
  fleet_logs {fleet, instance, tail?}               # container logs
  fleet_restore_backup                              # docs only: where state backups are + how to restore

LIFECYCLE  (async-first: returns {job_id, done:false} in seconds; wait:true blocks)
  fleet_up {fleet, instance, remote?, branch?, backend?, cluster?, settings?, wait?}   # settings: a new fleet's options
  fleet_clone {fleet, source, destination, display_name?, tag?, color?, branch?, wait?}
  fleet_rebuild {fleet, instance, wait?}            # recreate container, keep workspace; retries a failed cluster create/rebuild
  fleet_down {fleet, instance, wait?}               # remove one instance      (irreversible)
  fleet_destroy_fleet {fleet, wait?}                # remove fleet + instances (irreversible)
  fleet_job_status {job_id}                         # poll: running | succeeded | failed
  fleet_start {fleet, instance}                     # resume a stopped instance (blocks)
  fleet_stop {fleet, instance}                      # pause, keep workspace     (blocks)

EXECUTION
  fleet_exec {fleet, instance, command: [argv...]}  # one-shot; exit_code is data

FILES  (the daemon copies; bytes never pass through MCP)
  fleet_copy {src: {fleet?, instance?, path}, dst: {fleet?, instance?, path}}   # no fleet/instance = daemon host

SESSIONS  (interact with an agent / long-lived process in an instance)
  fleet_session_spawn {fleet, instance, session}
  fleet_session_exec {fleet, instance, session, command: "shell string"}
  fleet_session_read {fleet, instance, session, scrollback?}   # 0 screen, N last N, <0 all
  fleet_session_list {fleet, instance}

AUTOMATION  (unattended agents fired by triggers; every write returns {agents, triggers})
  fleet_automation_list {fleet}                                # read agents + triggers
  fleet_agent_create {fleet, name, command?, system_prompt?, backend?, cluster?}
  fleet_agent_update {fleet, name, new_name?, command?, system_prompt?, backend?, cluster?}   # omit a field to keep it
  fleet_agent_delete {fleet, name}                             # fails if a trigger references it
  fleet_trigger_create {fleet, name, type, agents:[...], prompt?, cron? (schedule) | cron?+script? (bash) | webhook_name?+filter_type?+regex?|json_path?+json_value? (webhook)}
  fleet_trigger_update {fleet, name, new_name?, type?, agents?, ...}                 # omit a field to keep it
  fleet_trigger_delete {fleet, name}
  fleet_trigger_logs {fleet, trigger}                          # recorded firing payloads (debug what fired it)

CUSTOM MOUNTS  (folders every instance of a fleet gets; taken at next create/rebuild)
  fleet_mount_list {fleet}
  fleet_mount_set {fleet, path, host_folder?}                  # add, or re-point to a daemon-host folder
  fleet_mount_delete {fleet, path}

FLEET OPTIONS  (the TUI's Edit fleet settings; a passed list replaces the list)
  fleet_settings {fleet}                                       # read every option
  fleet_edit {fleet, settings: {claude_code_mount?, codex_mount?, auggie_mount?, fleet_mcp?, gh_mount?, home_dir?,
      prefer_fleet_launch?, layout_presets?, custom_mounts?, buildkit_server?, deb_cache_server?,
      image_cache_server?, coder_workspace_name?, coder_template?, coder_preset?, coder_parameters?}}

KUBERNETES  (the daemon's cluster connections; cluster = id, name or kube context)
  fleet_cluster_list {cluster?}                                # health, settings, capabilities
  fleet_cluster_test {cluster, deep?, storage_classes?, test_storage?}   # run the checks now
```
