# fleet-man

A CLI/TUI tool for managing fleets of agents each inside there own isolated devcontainers. This tool enables hugly parallel development, especially if you utilize the `fleet-admiral` skill (auto added to your claude code on install). 

![fleet-man close up](fleet-closeup.png)
---
![fleet-man screenshot](fleet-showcase.png)


## What problem does `fleet` solve? 

I made this tool to solve my own problem, and hopefully yours as well. 
I wanted a tool that allowed the following.

1. Running unlimited parallel agents that don't step on each other. I mean 
this in a way beyond the `git worktree` I don't want ports to step on each 
other, I don't want one agent deleting the whole computer to step on another agent. I want full isolation. 
2. Use my claude code (or any agent you want) subscription at the subscription 
price! Many similar tools exist to this, but require you to pay API rates. This 
tool just uses terminals so you will never need to pay API rates.
3. Not need to constantly setup new environments for agents. Environments need 
automated setup.
4. Give me a one screen dashboard to manage all my agents, including knowing 
when they need attention.

`fleet` solves all these problems. I use it every day. A few other senior developers at my company use it every day. You should use it every day! 


## Contents

- [Install](#install)
- [Usage](#usage)
- [TUI Keybindings](#tui-keybindings)
- [MCP Server](#mcp-server)
- [Remote MCP](#remote-mcp) — expose MCP & gRPC to remote agents via a fleet gateway
- [Environment Variables](#environment-variables)
- [Requirements](#requirements)
- [Development](#development)
- [Windows / WSL Setup](#windows-wsl-setup)
- [Devcontainer Customizations](#devcontainer-customizations)
- [Fleet Launch (in-instance TUI)](#fleet-launch-in-instance-tui)
- [Devcontainer BuildKit](#devcontainer-buildkit)
- [Devcontainer UID Rewrite](#devcontainer-uid-rewrite)

**Deep-dive docs** (in [`doc/`](doc/)):

- [The Fleet Gateway](doc/gateway.md) — how the reverse tunnel relays both MCP and gRPC to your daemon

## Install

```bash
sudo curl -sL https://raw.githubusercontent.com/BenjaminBenetti/fleet-man/main/install.sh | sh
```

To install a specific version:

```bash
sudo curl -sL https://raw.githubusercontent.com/BenjaminBenetti/fleet-man/main/install.sh | sh -s -- --version v0.2.0
```

## Usage

Run `fleet` with no arguments to launch the interactive TUI, or use subcommands directly:

```bash
# Launch TUI
fleet

# Spawn instances from the current repo
fleet up agent-1
fleet up agent-2

# Stop and restart an existing instance without removing it
fleet stop agent-1
fleet start agent-1

# List instances
fleet ls

# Exec into an instance
fleet exec agent-1 bash

# Open VS Code on an instance
fleet code agent-1

# Launch the in-instance link/app grid (run inside an instance)
fleet launch

# View logs
fleet logs agent-1

# Rebuild an instance's container in place (e.g. after editing devcontainer.json)
fleet rebuild agent-1

# Remove an instance
fleet down agent-1

# Remove a fleet and all its instances
fleet destroy my-project

# Spawn from anywhere with explicit repo
fleet up agent-1 --repo git@github.com:org/my-project.git

# Reference an existing fleet from anywhere
fleet up my-project/agent-3

# Configure automation: agents (workers) and triggers (what fires them)
fleet agent create my-project nightly-builder --system-prompt "Build and report"
fleet trigger create my-project nightly --agent nightly-builder --cron "0 0 * * *" --prompt "Run the nightly build"
# A 'bash' trigger polls a command on its cron and fires only when it exits 0
# (stdout becomes the event payload) — for sources without a webhook
fleet trigger create my-project new-issues --type bash --agent triager --cron "*/5 * * * *" \
  --script 'gh issue list --label needs-triage --json number -q ".[].number" | grep -q .' --prompt "Triage the new issues"
fleet agent list my-project
fleet trigger list my-project
fleet trigger logs my-project nightly   # inspect a trigger's recorded firings
```

## TUI Keybindings

| Key | Action |
|-----|--------|
| `j/k` | Navigate |
| `space` | Expand/collapse fleet |
| `enter/e` | Exec into instance |
| `s` | Stop/start instance |
| `o` | Open instance in new terminal |
| `a` | Add instance |
| `n` | New fleet |
| `d` | Delete instance/fleet |
| `c` | Open VS Code |
| `R` | Rebuild instance (preserves workspace) |
| `L` | View logs (instance logs, or a selected trigger's event logs in the automation view) |
| `→/l`, `←/h` | Select / deselect the PR-status auto tag |
| `enter` (on PR status) | Open the PR in a browser |
| `A` | Switch armada (remote fleet) |
| `r` | Refresh |
| `q` | Quit |

## MCP Server

The fleet daemon also runs an [MCP](https://modelcontextprotocol.io) server over
Streamable HTTP, so AI agents (and any MCP client) can drive fleet
programmatically. It starts automatically with the daemon — no extra command.

- It binds `127.0.0.1:6012`, or the next free port if 6012 is taken.
- The active port is written to `~/.fleet/mcp.port`, so clients can discover the
  endpoint. The URL is `http://127.0.0.1:<port>`.
- Requests must carry `Authorization: Bearer <token>`, where the token is read
  from `~/.fleet/mcp.token`. The loopback port is reachable by any local user,
  so the token (file mode `0600`, same-user only) is the access boundary —
  matching the daemon's unix socket. The token is generated once and reused
  across restarts.

**Claude Code needs no setup:** launching the fleet TUI registers the server in
`~/.claude.json` as the user-scope `fleet` entry (URL and token included) and
re-syncs it on every launch, so a daemon restart that lands on a new port heals
itself. It also installs the **Fleet Admiral** skill (`~/.claude/skills/fleet-admiral`),
which teaches the agent how to drive these tools. (Local daemons only: when
`FLEET_GATEWAY`/`FLEET_SERVER` point the TUI at a remote daemon, registration
is skipped — point Claude Code at the Public MCP URL from the settings page
instead, see [Remote MCP](#remote-mcp). Registration is best-effort and never
blocks startup.) The snippet below is for other MCP clients or manual setups.

For convenience the server also writes `~/.fleet/mcp.env` (mode `0600`) with the
endpoint as shell exports, and wires `~/.bashrc` to source it, so new shells get:

```bash
FLEET_MCP_PORT=6012
FLEET_MCP_URL=http://127.0.0.1:6012
FLEET_MCP_TOKEN=<token>
```

An MCP client config (`mcp.json`) can then reference them directly:

```json
{
  "mcpServers": {
    "fleet": {
      "type": "http",
      "url": "${FLEET_MCP_URL}",
      "headers": { "Authorization": "Bearer ${FLEET_MCP_TOKEN}" }
    }
  }
}
```

(zsh users: add `[ -f "$HOME/.fleet/mcp.env" ] && . "$HOME/.fleet/mcp.env"` to
`~/.zshrc`.)

Tools mirror the non-interactive CLI: `fleet_list`, `fleet_status`,
`fleet_version`, `fleet_logs`, `fleet_up`, `fleet_start`, `fleet_stop`,
`fleet_down`, `fleet_destroy_fleet`, `fleet_clone`, `fleet_rebuild`, `fleet_exec`,
the tmux session tools `fleet_session_spawn` / `fleet_session_exec` /
`fleet_session_read` / `fleet_session_list`, and the automation CRUD tools
`fleet_automation_list`, `fleet_agent_create` / `fleet_agent_update` /
`fleet_agent_delete`, `fleet_trigger_create` / `fleet_trigger_update` /
`fleet_trigger_delete`, and `fleet_trigger_logs` (mirroring the `fleet agent` and
`fleet trigger` CLI commands).
Interactive, open-ended commands (`fleet shell`, log following) are intentionally
not exposed.

## Remote MCP

By default the MCP server is loopback-only. To let a **remote** agent drive your
fleet, you can expose it through a **fleet gateway** — a small public relay you
(or someone) runs. Your daemon dials *out* to the gateway (so it works behind
NAT/firewalls) and the gateway routes inbound MCP requests back down that tunnel:

```
agent ──HTTPS──▶ fleet gateway ──reverse tunnel──▶ your fleetd ──▶ local MCP server
```

> ⚠️ **Exposing fleet over the internet:** the bearer token is the only thing gating access (and over gRPC it grants *full* daemon control), and the gateway operator can see your traffic — use a gateway you trust and keep the token secret. See **[The Fleet Gateway](doc/gateway.md)** for the trust model.

### Enable it (in the TUI)

In **Settings → Fleet Remote (MCP)**:

1. Set **Gateway URL** to the gateway's **gRPC** endpoint, e.g.
   `https://gateway.example.com:50051` (this is where the daemon registers and
   dials out — the gateway's `--grpc-addr`, default port `50051`; behind a proxy
   it's whatever host:port routes to that listener). It is *not* the public address
   agents use.
2. Flip **Enable Remote MCP** on.

Once connected, the read-only **Public MCP URL** appears (e.g.
`https://gateway.example.com/mcp/<id>`) — that is the address external tools use.
The line shows the live connection state (connecting / connected / error).

A second, independent toggle — **Enable Remote Fleet** — exposes the daemon's
**gRPC control surface** through the same gateway, so a remote `fleet` binary can
drive this instance (see [Remote control](#remote-control-grpc)). With it off,
fleetd never negotiates the gRPC tunnel feature, so the gateway rejects any
incoming gRPC commands aimed at the daemon. With it on, a read-only **Public GRPC
URL** appears under the Public MCP URL (computed by the gateway from its
`--public-grpc-url` flag) — that value is what you feed another `fleet` as
`FLEET_GATEWAY`. The toggles are independent: expose MCP, remote control, or both.

A third, independent toggle — **Enable Webhook** — exposes this daemon's
**automation webhook** endpoint through the same gateway. With it on, a read-only
**Public Webhook URL** base appears (e.g. `https://gateway.example.com/webhook/<id>`),
served on the gateway's public HTTP listener — no extra gateway flag needed. A
remote system (CI, a SaaS webhook, `curl`) POSTs an event to
`<public-webhook-url>/<name>`, where `<name>` is a [webhook trigger](#automation)
you defined; the gateway relays it down the tunnel and fleetd fires that trigger's
agents if the event passes the trigger's filter (a regex over the body, or a JSON
path/value match). The trigger's create/edit dialog renders its full URL ready to
paste into the remote system. With the toggle off, fleetd never negotiates the
webhook feature, so the gateway 404s the route for this daemon.

### Use it from a remote agent

Point the remote MCP client at the Public MCP URL, with the **same bearer token**
your daemon uses (from `~/.fleet/mcp.token`):

```json
{
  "mcpServers": {
    "fleet-remote": {
      "type": "http",
      "url": "https://gateway.example.com/mcp/<id>",
      "headers": { "Authorization": "Bearer <token from ~/.fleet/mcp.token>" }
    }
  }
}
```

### Running a gateway

The gateway is the same binary: `fleet gateway`. TLS is **optional**: give it a
**publicly-trusted** certificate (e.g. from Let's Encrypt) and it terminates TLS
itself, or omit the cert and run it behind a TLS-terminating reverse proxy (see
[below](#behind-a-tls-terminating-reverse-proxy)).

```bash
fleet gateway \
  --public-url https://gateway.example.com \
  --tls-cert /etc/fleet/tls/fullchain.pem \
  --tls-key  /etc/fleet/tls/privkey.pem
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--public-url` | (required) | External base URL agents use; session URLs are `<public-url>/mcp/<id>`. Scheme (`https`/`http`) must match how the public endpoint is actually served |
| `--public-grpc-url` | (optional) | External base URL of the gRPC endpoint remote `fleet` clients dial, e.g. `https://gateway.example.com:50051`. Daemons that enable **Remote Fleet** are handed `<public-grpc-url>/grpc/<id>` as their **Public GRPC URL** (shown in the TUI, used as `FLEET_GATEWAY`). Unset = no URL is computed |
| `--tls-cert` / `--tls-key` | (optional) | TLS certificate + key (PEM). Provide **both** to serve HTTPS, or **neither** for plain HTTP behind a proxy. A lone cert or key is an error |
| `--public-addr` | `:443` | MCP + `/healthz` listener — HTTP/1.1 (HTTPS when a cert is set, else HTTP) |
| `--grpc-addr` | `:50051` | Native gRPC listener — HTTP/2 (h2c when cert-less, h2 under TLS). Hosts remote `fleet` control **and** fleetd registration. Empty disables both |
| `--max-sessions` | `1024` | Cap on concurrent tunnels |
| `--session-key` | (random per boot) | Secret key signing the session-resume tokens daemons present on reconnect. Set it (or `FLEET_GATEWAY_SESSION_KEY`) so daemons keep the **same session URL across gateway restarts**; left unset, a restart hands every daemon a fresh URL |

Every flag is also settable via environment variable — `FLEET_GATEWAY_<FLAG>` with
dashes as underscores (e.g. `FLEET_GATEWAY_PUBLIC_URL`,
`FLEET_GATEWAY_MAX_SESSIONS`) — which is handy for configuring the
[Docker image](#run-it-with-docker) in Kubernetes. A flag given on the command
line wins over its environment variable.

Two ports must be reachable: expose `--public-addr` to MCP agents and `--grpc-addr`
to remote `fleet` clients **and your daemons** (they register over it). A
`GET /healthz` on the public listener returns `ok`.

> **There is no separate control port.** fleetd registers and carries its reverse
> tunnel over a long-lived gRPC bidi stream on `--grpc-addr` (HTTP/2), so the whole
> gateway is plain HTTP/HTTP-2 — no raw TCP.

#### Behind a TLS-terminating reverse proxy

Both listeners are **L7** — there's no raw-TCP port anymore, so a standard HTTP/gRPC
ingress fronts the entire gateway:

- **`--public-addr` (MCP, HTTP/1.1)** → a Traefik **HTTP** `IngressRoute` (TLS-terminating).
- **`--grpc-addr` (gRPC, HTTP/2)** → a Traefik **gRPC** route (h2c backend) — see
  [Traefik's gRPC guide](https://doc.traefik.io/traefik/user-guides/grpc/). This one
  carries both remote `fleet` control RPCs **and** fleetd registration (the same
  long-lived bidi stream).

Run the gateway with **no** `--tls-cert`/`--tls-key` (plain HTTP/h2c) and let the
proxy terminate TLS. For example:

```yaml
# MCP — L7 HTTP, TLS terminated at Traefik:
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata: { name: fleet-gateway-mcp }
spec:
  entryPoints: [websecure]
  routes:
    - match: Host(`gateway.example.com`) && PathPrefix(`/mcp`)
      services: [{ name: fleet-gateway, port: 80 }]            # gateway --public-addr
  tls: { secretName: gateway-tls }
---
# gRPC (remote control + fleetd registration) — L7 gRPC, h2c to the backend:
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata: { name: fleet-gateway-grpc }
spec:
  entryPoints: [websecure]
  routes:
    - match: Host(`grpc.gateway.example.com`)
      services: [{ name: fleet-gateway, port: 50051, scheme: h2c }] # gateway --grpc-addr
  tls: { secretName: gateway-tls }
```

Set `--public-url https://gateway.example.com`; the daemon's **Gateway URL** is the
gRPC endpoint, e.g. `https://grpc.gateway.example.com` (or `https://gateway.example.com:50051`
without a proxy). Clients (and daemons) verify TLS against the system roots at the
proxy edge; the proxy speaks plain HTTP/h2c to the cert-less gateway, which can then
bind unprivileged ports and needs no cert.

#### Run it with Docker

Every tagged release also publishes a gateway image to the GitHub Container
Registry, so you don't have to build the binary yourself. It's the same
`fleet gateway`, with the flags passed as `docker run` arguments (or as
`FLEET_GATEWAY_*` environment variables — see the table above):

```bash
docker run -d --name fleet-gateway \
  -p 443:443 -p 50051:50051 \
  -v /etc/fleet/tls:/tls:ro \
  ghcr.io/benjaminbenetti/fleet-man/gateway:latest \
  --public-url https://gateway.example.com \
  --tls-cert /tls/fullchain.pem \
  --tls-key  /tls/privkey.pem
```

Image tags track releases: `:latest` (newest stable), `:X.Y.Z`, and `:X.Y`.
Images are multi-arch (`linux/amd64` + `linux/arm64`). To run behind a
TLS-terminating proxy, drop the `-v .../tls` mount and the `--tls-cert`/`--tls-key`
flags (and a `--public-url http://…` or proxy-fronted `https://…`); the container
then serves plain HTTP.

### Security model

- **No gateway authentication.** Anyone can open a tunnel; the gateway just routes
  bytes. Isolation comes from the **unguessable 256-bit id** in the public URL.
- **The bearer token is the real access boundary.** The gateway forwards the
  `Authorization` header untouched to your loopback MCP server, whose existing
  token check gates every request. Treat the Public MCP URL as a capability, but
  **the token is the secret** — share both only with agents you trust.
- The id in the URL is *not* the reconnect credential: the daemon holds a separate
  secret (never placed in the URL), so a URL holder cannot hijack your tunnel.
- **Session-resume tokens are bearer reclaim capabilities.** Each registration
  returns a JWT signed with `--session-key`; the daemon stores it (mode `0600`,
  next to the secret) and presents it on reconnect, which is what keeps the URL
  stable across gateway restarts. Anyone holding the token can claim that session,
  so guard the signing key like any other server secret.
- The public connection is TLS when the gateway holds a cert (the daemon verifies
  it against the system roots), or when a reverse proxy terminates TLS in front of
  it. Running the gateway itself on plain HTTP is only safe behind such a proxy (or
  on a trusted private network) — never expose a cert-less gateway directly.

### Remote control (gRPC)

The same gateway tunnel can also carry the daemon's **gRPC** API, so a remote
`fleet` client can drive your daemon directly — list/up/down, watch live status,
stream logs, change config, and so on. It rides the **same tunnel** as remote MCP
but has its **own toggle**: flip **Enable Remote Fleet** in Settings → Fleet MCP
(independent of Enable Remote MCP, so you can expose MCP, remote control, or
both). The gateway serves native gRPC on its dedicated `--grpc-addr` listener
(default `:50051`); the client dials it as an ordinary gRPC endpoint and sends the
daemon's session id (the same id as in the MCP URL) as the `fleet-session`
metadata header, so the gateway can route.

Point a `fleet` client at it with two env vars — the URL is the gRPC endpoint plus
the session id. When the gateway runs with `--public-grpc-url`, the settings page
shows this exact value as the **Public GRPC URL** (e.g.
`https://gateway.example.com:50051/grpc/<id>`); otherwise compose it from the
gateway's `--grpc-addr` (or whatever host:port your reverse proxy exposes for
gRPC) plus the session id:

```bash
export FLEET_GATEWAY="https://gateway.example.com:50051/<id>"
export FLEET_TOKEN="<token from ~/.fleet/mcp.token>"   # omit on the same host as the daemon
fleet ls          # …now talks to the REMOTE daemon
```

`FLEET_GATEWAY` takes precedence over `FLEET_SERVER` and the local socket; a remote
endpoint is never auto-spawned, and a daemon/client version mismatch is a hard
error. Use `http://…` instead of `https://…` only when the gateway serves plain
h2c with no TLS in front of it (a trusted private network).

> ⚠️ **Security — this is full remote control.** The bearer token authorizes
> *every* gRPC call, so the **same `~/.fleet/mcp.token` now grants full control of
> your daemon over the internet** — not just the read-mostly MCP tools. The gateway
> terminates TLS, so its operator can see your traffic and token (the same trust
> model as remote MCP). Only enable this with a gateway you run or trust, and treat
> the token as a high-value secret.

> **Note:** interactive `fleet exec` (a remote shell) is **not yet supported** over
> the gateway — it currently runs the command on the *client's* host. Every other
> RPC works remotely today; remote interactive shell awaits a server-side `Exec`
> handler.

### Fleet Armada (multiple remote fleets)

Instead of juggling `FLEET_GATEWAY`/`FLEET_TOKEN` by hand, register remote fleets
once in **Settings → Fleet Armada**: press **+ Remote Fleet**, paste the gateway
URL and the remote's bearer token, and fleet runs a connection test before
saving. Each registered remote shows a live connection status (pinged while the
settings page is open) and a `[ delete ]` button (press twice to confirm).

The main page's list border carries an **Armada selector**
(`╭─ Armada [ local ] ───╮`). It is part of the normal `j`/`k` (and arrow-key)
navigation — cycle up past the top row to land on it (continuing up wraps to the
bottom of the list) — or jump straight to it with the **`A`** key or a mouse
click. Selecting it opens a dropdown of `local` + every registered remote; pick
one to **switch the TUI live**: the connection, fleet list, sessions, and shells
all retarget to the chosen daemon. Registering a remote never switches to it;
every boot starts on `local` unless `FLEET_GATEWAY` (or `FLEET_SERVER`) is set,
in which case that boot endpoint appears in the dropdown as `(env)`.

Remotes are shown by **hostname** rather than the full gateway URL; if two
registered fleets live on the same host, they're disambiguated by the first 8
characters of their session id (`fleet.example.com - 8e7d1f0a`).

The registry is stored on your own machine at `~/.fleet/armada.json` (mode
`0600` — it holds bearer tokens) and always round-trips through your **local**
daemon, even while the TUI is connected to a remote fleet.

## Environment Variables

Variables fleet **reads** (set them to configure behavior):

| Variable | Values / format | What it does |
|----------|-----------------|--------------|
| `FLEET_GATEWAY` | `https://gw:50051/<id>` (or `http://…` for a cert-less/h2c gateway) | Drive a *remote* daemon through a fleet gateway (full gRPC control). Takes precedence over `FLEET_SERVER` and the local socket. See [Remote MCP](#remote-mcp). |
| `FLEET_TOKEN` | bearer token | Token for `FLEET_GATEWAY`. Defaults to `~/.fleet/mcp.token` on the daemon's own host. |
| `FLEET_SERVER` | `host:port` | Drive a remote daemon over plain TCP (no gateway). |
| `FLEET_DEVCONTAINER_BUILDKIT` | `auto` (default), `never` | BuildKit mode for Fleet-managed devcontainers. See [Devcontainer BuildKit](#devcontainer-buildkit). |
| `FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID` | `default`, `never`, `on`, `off` | Remote-user UID/GID rewrite mode. See [Devcontainer UID Rewrite](#devcontainer-uid-rewrite). |
| `FLEET_SSH_AGENT_SOCK` | path, `off`, or `none` | Override the bind source for SSH agent forwarding into instances (`off`/`none` disables it). On macOS the default is Docker Desktop's VM-side `/run/host-services/ssh-auth.sock` (OrbStack and `colima --ssh-agent` are path-compatible); set this if your Docker backend exposes the agent elsewhere (default Colima, Podman machine, Rancher Desktop). |
| `CODER_URL` | URL | Coder deployment URL (Coder backend). |
| `CODER_SESSION_TOKEN` | token | Coder API token (Coder backend). |
| `CODER_CONFIG_DIR` | path | Override the Coder CLI config dir. |

Variables fleet **exports** for MCP clients (written to `~/.fleet/mcp.env`, sourced from `~/.bashrc`):

| Variable | What it does |
|----------|--------------|
| `FLEET_MCP_URL` | The loopback MCP endpoint, `http://127.0.0.1:<port>`. |
| `FLEET_MCP_TOKEN` | The MCP bearer token. |
| `FLEET_MCP_PORT` | The MCP server's port. |

Fleet also **respects** standard environment when present: `HOME` (the `~/.fleet`
location), `TMUX` (enables split-pane mode when run inside tmux), `SSH_AUTH_SOCK`
(forwarded into instances for SSH/git), and `WSL_DISTRO_NAME` / `WSL_INTEROP`
/ `WAYLAND_DISPLAY` (platform detection for clipboard and browser integration).

## Requirements

- Linux (including Ubuntu on WSL2) or macOS
- Docker
- [devcontainer CLI](https://github.com/devcontainers/cli) (`npm install -g @devcontainers/cli`)

## Development

Building from source, tests, and the protobuf workflow: see [DEVELOPMENT.md](DEVELOPMENT.md).

## Windows WSL Setup

Fleet is a Linux CLI, but it can run on Windows through WSL2. The confirmed
setup is:

- Ubuntu on WSL2
- Docker Desktop with WSL integration enabled for the Ubuntu distro
- Node installed with `nvm`
- `@devcontainers/cli` installed under the nvm-managed Node
- Fleet installed somewhere on the WSL `PATH`, such as `~/.local/bin/fleet`

Inside WSL:

```bash
nvm install 22
nvm alias default 22
nvm use default
npm install -g @devcontainers/cli
```

On Docker Desktop plus WSL, if devcontainer startup fails while building the
UID-adjustment or feature image, disable BuildKit for Fleet-managed
devcontainers. If the failure is specifically in `updateUID.Dockerfile`,
disable the devcontainer CLI's remote user UID rewrite as well:

```bash
export FLEET_DEVCONTAINER_BUILDKIT=never
export FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID=never
```

To persist that setting:

```bash
echo 'export FLEET_DEVCONTAINER_BUILDKIT=never' >> ~/.bashrc
echo 'export FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID=never' >> ~/.bashrc
```

See [Windows WSL notes](docs/windows-wsl-notes.md) for a full health check and
disposable smoke-test workflow.

## Devcontainer Customizations

Fleet reads project-level settings from a `customizations.fleet` block in a
repo's `devcontainer.json`. This follows the standard devcontainer pattern
where each tool owns a namespaced sub-object under `customizations` (VS Code
uses `vscode`, GitHub uses `codespaces`, and so on) — Fleet reads `fleet` and
ignores the rest.

```jsonc
{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "customizations": {
    "fleet": {
      "browser": {
        "initialUrl": "http://localhost:3000"
      },
      "fleetLaunch": {
        "sites": [
          {
            "title": "API",
            "subTitle": "REST backend",
            "url": "http://localhost:3000",
            "healthCheck": "http://localhost:3000/healthz"
          }
        ],
        "apps": [
          {
            "title": "Logs",
            "command": "docker run -d -p 16768:8080 -v /var/run/docker.sock:/var/run/docker.sock amir20/dozzle:latest",
            "port": 16768
          }
        ]
      }
    }
  }
}
```

### `browser.initialUrl`

The address the built-in browser (`b` in the TUI) opens to instead of
`about:blank`. Handy for jumping straight to a dev server or app running
inside the instance.

### `fleetLaunch.sites`

A directory of links to the dev services running inside the instance. Each
entry has:

- `title` — the primary label for the link.
- `subTitle` — secondary descriptive text shown under the title (optional).
- `url` — the address the link navigates to.
- `icon` — an image shown before the title, used verbatim as an `<img>`
  source so any path the browser can load works, e.g. an `https` URL or a
  `data:` URI (optional).
- `healthCheck` — an address polled to indicate whether the service is
  reachable; a healthy service shows a green heart, an unreachable one a red
  skull, and hovering reveals the HTTP status (optional).

### `fleetLaunch.apps`

Embedded apps shown as extra tabs on the Fleet Launch page, alongside the
Links tab. Each app gets its own tab; opening it starts the app inside the
instance and embeds it in an iframe. This is how you surface a web UI that
lives in the instance — a log viewer, a database console, a build
dashboard — without leaving Fleet Launch. Each entry has:

- `title` — the label shown on the app's tab.
- `command` — a bash command that starts the app. It runs the first time
  the tab is opened, unless the app's `port` is already answering (so a
  second open or a browser relaunch won't double-start it). The command is
  started in the background, so it can be a blocking server or a
  self-detaching one like `docker run -d`. Optional — omit it for an app
  that is already running and only needs to be embedded.
- `port` — the localhost port the app serves on. Once it answers, the tab
  iframes `http://localhost:<port>`; if it never comes up, the tab shows an
  error instead.

The app is shown in an iframe, so it must allow being framed. An app that
sends `X-Frame-Options: deny`/`sameorigin` or a restrictive
`Content-Security-Policy: frame-ancestors` will render as a blank "refused
to connect" box; configure it to permit embedding. For example, Grafana
needs `GF_SECURITY_ALLOW_EMBEDDING=true`.

For example, to replace Fleet's former built-in Dozzle log viewer:

```jsonc
"apps": [
  {
    "title": "Logs",
    "command": "docker run -d -p 16768:8080 -v /var/run/docker.sock:/var/run/docker.sock amir20/dozzle:latest",
    "port": 16768
  }
]
```

### Precedence: `initialUrl` vs. Fleet Launch

When a devcontainer.json sets **both** `initialUrl` and a Fleet Launch
block (any `fleetLaunch.sites` or `fleetLaunch.apps`), the per-fleet
**Prefer Fleet Launch** setting decides which the browser opens to:

- off — `initialUrl` wins.
- on — the Fleet Launch page wins.

The **first** time you open the browser on a fleet whose workspace has both
configured, the TUI prompts you to choose, and saves your answer as the
fleet's Prefer Fleet Launch setting. You can change it later by editing the
fleet (`e` in the TUI). When only one of the two is configured, that one is
used and the setting has no effect.

## Fleet Launch (in-instance TUI)

`fleet launch` is a small terminal UI you run **inside an instance** (over
`fleet exec`, in a `fleet code` terminal, or any shell in the container). It
reads the workspace's `customizations.fleet.fleetLaunch` block — the same
`sites` and `apps` described above — and lays them out as a navigable grid of
squares: a "Links" section for `fleetLaunch.sites` and an "Apps" section for
`fleetLaunch.apps`.

```bash
# auto-detect .devcontainer/devcontainer.json (or ./devcontainer.json) in cwd
fleet launch

# or point it at an explicit devcontainer.json
fleet launch --config ./path/to/devcontainer.json

# print the configured links and apps (and the names you can launch), then exit
fleet launch list

# open a link or app directly by name (a unique prefix is enough), as if clicked
fleet launch graf
```

In the grid, navigate with the arrow keys or `hjkl`, or click a square with the
mouse; `enter` or a click activates the selected square, and `q`/`esc`/`ctrl+c`
quits. Activating a **link** opens the host browser to its `url`; activating
an **app** first starts the app's `command` on its `port` inside the instance
(only if the port isn't already answering), waits for it to come up, then
opens the host browser to `http://localhost:<port>`.

`fleet launch list` prints the configured Links and Apps with their targets and
exits — handy for discovering the names. `fleet launch <name>` performs that
same link/app activation headlessly, without opening the grid. The name is
matched case-insensitively against the titles: an exact title wins, otherwise a
unique prefix is enough (so `fleet launch graf` opens "Grafana"). If a prefix
matches more than one, the candidates are listed so you can type more.

The browser lives on the **host** (it is proxied into the container by
privoxy), so the in-instance TUI can't open it directly. Instead it drives the
host browser over a **control socket** — a unix domain socket the host `fleet`
TUI creates per instance and bind-mounts into the instance (the same mechanism
as mounting `docker.sock`). The running host `fleet` TUI listens on that socket
and opens or navigates the browser when `fleet launch` sends it a request. If
the host `fleet` TUI isn't running, `fleet launch` still renders the grid so
you can browse the configured options, but shows a status line noting that
opening won't work until a host connection exists.

Only instances **created or cloned after** this feature was added get the
control-socket mount; pre-existing instances need to be recreated for
in-instance launch to drive the host browser.

## Devcontainer BuildKit

Fleet uses the devcontainer CLI's default BuildKit behavior unless explicitly
configured. If Docker Desktop on WSL fails while building the devcontainer
UID-adjustment image, disable BuildKit for Fleet-managed devcontainers:

```bash
export FLEET_DEVCONTAINER_BUILDKIT=never
```

Accepted values are `auto` and `never`.

## Devcontainer UID Rewrite

Fleet uses the devcontainer CLI's default remote-user UID/GID rewrite behavior
unless explicitly configured. On Docker Desktop plus WSL, that rewrite can fail
while building `updateUID.Dockerfile`. To disable it for Fleet-managed
devcontainers:

```bash
export FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID=never
```

Accepted values are `default`, `never`, `on`, and `off`.
