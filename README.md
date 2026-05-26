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
        "initialUrl": "http://localhost:3000",
        "landingPage": {
          "sites": [
            {
              "title": "API",
              "subTitle": "REST backend",
              "url": "http://localhost:3000",
              "healthCheck": "http://localhost:3000/healthz"
            }
          ]
        }
      }
    }
  }
}
```

### `browser.initialUrl`

The address the built-in browser (`b` in the TUI) opens to instead of
`about:blank`. Handy for jumping straight to a dev server or app running
inside the instance.

### `browser.landingPage.sites`

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

### Precedence: `initialUrl` vs. the landing page

When a devcontainer.json sets **both** `initialUrl` and a `landingPage.sites`
list, the per-fleet **Prefer Fleet Launch** setting decides which the browser
opens to (edit a fleet with `e` in the TUI):

- off (default) — `initialUrl` wins.
- on — the Fleet Launch landing page wins.

When only one of the two is configured, that one is used and the setting has
no effect.

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
