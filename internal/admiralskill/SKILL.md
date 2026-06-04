---
name: fleet-admiral
description: Gives you instructions on how to use the fleet tool. Use this when you need to know how to use fleet
or the user is asking you to interact with fleet in some way, especially in the case of kicking of multiple agents 
in fleet instances.
---

# Fleet Admiral

`fleet` manages a fleet of isolated dev instances — each a devcontainer with its
own checkout of a repository — and can run a coding agent inside any of them.
This skill gives you an understanding of what fleet is and how to drive its CLI,
so you can put it to use however a task calls for. It does not prescribe a
workflow: you decide how to apply these capabilities to what the user asks.

## Concepts

```
   fleet  (a repository)
     ├── instance A   ← isolated devcontainer, its own checkout
     │     └── agent  ← a coding agent can run inside (driven via a session)
     ├── instance B
     └── instance C
```

- **Fleet** — a group of instances tied to one repository. Derived from the repo
  (its name comes from the repo URL basename).
- **Instance** — one devcontainer: an isolated workspace you can run
  commands in, open in an editor, or run an agent inside. It has a lifecycle
  (`creating → running ⇄ stopped → deleting`, plus `failed`).
- **Session** — a named, *detached* tmux session inside an instance. Sessions are
  how you interact non-interactively with something long-lived in an instance
  (most importantly, a coding agent): you create the session, send keystrokes to
  it, and read back its current screen. It persists across separate CLI calls
  until you stop the instance or kill it.

## Addressing instances

Most commands take an instance reference: `<fleet>/<instance>` 

## What the CLI can do

**Inspect state.** `fleet status` prints fleet-wide summary counts;
`fleet list [fleet]` prints a per-instance table (name, status, branch,
container, created). Output is human-readable text, not JSON — parse the columns
if you need to act on it programmatically.

**Manage instance lifecycle.**

- `fleet up <name> [--repo URL] [--branch BRANCH] [--backend devcontainer|coder|codespaces]`
  creates and starts an instance. The repo is resolved from `--repo`, the
  fleet's recorded remote, or the current directory's git remote. `--backend`
  selects where it runs (Docker devcontainer by default; also Coder or GitHub
  Codespaces). Backends need to be configured, so unless specified by user, prefer devcontainer backend
- `fleet start <name>` / `fleet stop <name>` resume / pause an instance. Stop
  preserves the workspace; start brings it back.
- `fleet down <name>` stops and removes a single instance. `fleet destroy <fleet>`
  removes a whole fleet and all its instances. **Both are irreversible** and
  discard any uncommitted work in those instances.

**Run a one-off command.** `fleet exec <name> <command...>` runs a command inside
an instance and inherits your terminal (e.g. `fleet exec api go test ./...`). Use
this when you don't need anything persistent.

> **Passing flags to your command — use `--`.** `fleet` parses flags *before* the
> command you want to run, so any `-x` / `--flag` belonging to that command is
> otherwise swallowed by fleet itself and you get `Error: unknown flag: --foo`.
> Put a `--` after the instance (and session) arguments; everything after it is
> passed through verbatim:
>
> ```bash
> fleet exec <instance> -- git log --oneline          # NOT: fleet exec <instance> git log --oneline
> fleet exec-in-session <instance> <session> -- npm test --watch
> ```
>
> This bites the common cases — `-la`, `--oneline`, `-f`, `-n 5`. For
> `exec-in-session` you can equivalently quote the whole command as a single
> argument (`... <session> "npm test --watch"`), since it joins the remaining
> args into one string before sending them; for `fleet exec`, prefer `--` so the
> command's arguments stay as separate argv elements.

**Drive an agent (or any long-lived process) via sessions.** This is the
non-interactive loop for working with a coding agent inside an instance:

```bash
fleet spawn-session <instance> <session>                 # create a detached tmux session
fleet exec-in-session <instance> <session> "<command>"   # type a command + Enter into it
fleet read-session <instance> <session> [--scrollback N] # capture the session's screen
```

`exec-in-session` sends keystrokes (it does not wait for completion), so to
follow progress you read the session back. `read-session --scrollback N` returns
the last N lines (`0` = just the visible screen, `-1` = full history). To put an
agent to work, launch the agent in the session with the task as its prompt, then
read the session to see what it's doing or asking.

## Behaviors worth knowing

- Commands return exit code `0` on success, non-zero on failure, and print
  errors to stderr.
- `exec`, `shell`, and the `*-session` commands attach to your TTY / send raw
  keystrokes — they assume an interactive terminal model.
- Sessions are detached and survive between CLI invocations; reading one is a
  snapshot of its current screen, not a stream.
- An instance must be `running` to exec into it or use its sessions; `start` a
  `stopped` one first.
- `fleet` parses its own flags first: separate the command you want to run with
  `--` (see "Passing flags to your command") or its flags will be intercepted.

## Command reference

```
LIFECYCLE
  fleet up <name> [--repo URL] [--branch BRANCH] [--backend devcontainer|coder|codespaces]
  fleet start <name>            # resume a stopped instance
  fleet stop <name>             # pause, keep workspace
  fleet down <name>             # stop + remove one instance        (irreversible)
  fleet destroy <fleet>         # remove a fleet and all instances  (irreversible)
  fleet clone <src> <dst>       # duplicate an instance (keeps installed state)

INTROSPECTION
  fleet status                  # fleet-wide summary counts
  fleet list [fleet]            # per-instance table (name, status, branch, ...)

EXECUTION  (use `--` before a command that has its own flags)
  fleet exec <name> -- <cmd...>           # e.g. fleet exec api -- git log --oneline

SESSIONS (interact with an agent / long-lived process in an instance)
  fleet spawn-session <instance> <session>
  fleet exec-in-session <instance> <session> -- <cmd...>   # or quote: "<cmd>"
  fleet read-session <instance> <session> [--scrollback N]   # 0 = screen, N = last N, -1 = all

ADDRESSING
  <instance>           # when run inside the repo's directory (fleet inferred)
  <fleet>/<instance>   # from anywhere
```
