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
  commands in, open in an editor, or run an agent inside. It has a lifecycle
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
names its instances `source` and `destination`.) Use `fleet_list` or
`fleet_status` to discover the names when you don't know them.

## What the tools can do

All tools return structured JSON — no text output to parse.

**Inspect state.** `fleet_status` summarizes every fleet (instance counts by
running/stopped/other, plus the repo remote). `fleet_list` returns per-instance
records (fleet, instance, status, branch, tag, backend, container_id,
workspace_dir, created_at, error), optionally filtered to one fleet. `fleet_version` reports
the daemon's version and liveness. `fleet_logs` returns an instance's container
logs (use `tail` to cap to the last N lines).

**Manage instance lifecycle.** These block until the job finishes server-side
(`fleet_up` provisions a devcontainer — expect minutes, not seconds) and return
`{success, instance, warnings}`; a failed job surfaces as a tool error.

- `fleet_up {fleet, instance, remote?, branch?, backend?}` creates and starts an
  instance. `remote` (a git URL) is required only when creating the first
  instance of a new fleet; after that the fleet's recorded remote is used.
  `backend` selects where it runs: `devcontainer`, `coder`, or `codespaces`.
  Omitted, it falls back to the host's configured default backend
  (`devcontainer` out of the box) — the others need prior configuration, so
  leave it unset or pass `devcontainer` unless the user says otherwise.
- `fleet_start` / `fleet_stop {fleet, instance}` resume / pause an instance.
  Stop preserves the workspace; start brings it back.
- `fleet_down {fleet, instance}` stops and removes a single instance.
  `fleet_destroy_fleet {fleet}` removes a whole fleet and all its instances.
  **Both are irreversible** and discard any uncommitted work in those instances.
- `fleet_clone {fleet, source, destination, ...}` duplicates an instance,
  keeping its installed state (optional `display_name`, `tag`, `color`,
  `branch` overrides).

**Run a one-off command.** `fleet_exec {fleet, instance, command}` runs a
command inside an instance and returns `{stdout, stderr, exit_code}`. `command`
is an argv **array**, not a shell string — `["git", "log", "--oneline"]` — so
there is no quoting or flag-parsing to worry about. A non-zero exit comes back
as data, not a tool error: check `exit_code`. Not for long-running or
interactive commands — use a session for those.

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

## Behaviors worth knowing

- An instance must be `running` to exec into it or use its sessions;
  `fleet_start` a `stopped` one first.
- Sessions are detached and survive between tool calls; reading one is a
  snapshot of its current screen, not a stream.
- Errors come back as tool errors with a clear message (instance not found, not
  running, job failed); the one exception is `fleet_exec`, where a non-zero
  exit is ordinary result data.
- Lifecycle `warnings` are non-fatal issues from an otherwise-successful job —
  surface them, don't treat them as failure.
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

LIFECYCLE  (block until the job completes; up can take minutes)
  fleet_up {fleet, instance, remote?, branch?, backend?}
  fleet_start {fleet, instance}                     # resume a stopped instance
  fleet_stop {fleet, instance}                      # pause, keep workspace
  fleet_down {fleet, instance}                      # remove one instance      (irreversible)
  fleet_destroy_fleet {fleet}                       # remove fleet + instances (irreversible)
  fleet_clone {fleet, source, destination, display_name?, tag?, color?, branch?}

EXECUTION
  fleet_exec {fleet, instance, command: [argv...]}  # one-shot; exit_code is data

SESSIONS  (interact with an agent / long-lived process in an instance)
  fleet_session_spawn {fleet, instance, session}
  fleet_session_exec {fleet, instance, session, command: "shell string"}
  fleet_session_read {fleet, instance, session, scrollback?}   # 0 screen, N last N, <0 all
  fleet_session_list {fleet, instance}
```
