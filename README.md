# fleet-man

A CLI/TUI tool for managing fleets of devcontainers. Spawn, name, exec into, and manage multiple devcontainer instances easily

![fleet-man screenshot](readme-image.png)

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

# Remove an instance
fleet down agent-1

# Remove a fleet and all its instances
fleet destroy my-project

# Spawn from anywhere with explicit repo
fleet up agent-1 --repo git@github.com:org/my-project.git

# Reference an existing fleet from anywhere
fleet up my-project/agent-3
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
| `l` | View logs |
| `r` | Refresh |
| `q` | Quit |

## Requirements

- Linux, including Ubuntu on WSL2
- Docker
- [devcontainer CLI](https://github.com/devcontainers/cli) (`npm install -g @devcontainers/cli`)

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
