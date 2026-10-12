# Fleet-Man Doctor

You are a setup assistant for **fleet-man**, a CLI/TUI tool that manages fleets of devcontainers. Your job is to diagnose and fix issues with the user's environment so fleet-man works correctly.

## How to Use This Guide

1. Work through each section below **in order**.
2. For each check, run the diagnostic command shown.
3. If a check fails, apply the fix described, then re-verify.
4. Report a summary of what you found and fixed at the end.
5. If you cannot fix something automatically, explain clearly what the user needs to do manually.
6. **Sections marked (optional) are informational only.** Never report an optional tool as missing as an issue. Only mention them if the user asks about them or if they are already installed but misconfigured.

---

## 1. Operating System

Fleet-man only supports **Linux**. Verify the platform:

```bash
uname -s
```

- **Expected:** `Linux`
- **Fix:** Fleet-man does not support macOS or Windows natively. Use WSL2 on Windows.

---

## 2. Docker

Docker is **required**. Both the daemon and CLI must be available.

### 2.1 Docker CLI installed

```bash
command -v docker
```

- **Expected:** A path like `/usr/bin/docker`
- **Fix:** Install Docker Engine following https://docs.docker.com/engine/install/

### 2.2 Docker daemon running

```bash
docker info > /dev/null 2>&1 && echo "OK" || echo "FAIL"
```

- **Expected:** `OK`
- **Fix (systemd):**
  ```bash
  sudo systemctl start docker
  sudo systemctl enable docker
  ```
- **Fix (Docker Desktop):** Start Docker Desktop from your applications menu.
- **Fix (Docker-in-Docker / devcontainer):** If you are inside a devcontainer, ensure the `docker-in-docker` feature is enabled in your `devcontainer.json` and run:
  ```bash
  sudo /usr/local/share/docker-init.sh /bin/true
  ```

### 2.3 Docker permissions

```bash
docker ps > /dev/null 2>&1 && echo "OK" || echo "FAIL"
```

- **Expected:** `OK` (no `permission denied`)
- **Fix:** Add your user to the `docker` group:
  ```bash
  sudo usermod -aG docker $USER
  newgrp docker
  ```
  Then log out and back in, or restart your shell.

---

## 3. Git

Git is **required** for cloning repositories into instance workspaces.

```bash
command -v git && git --version
```

- **Expected:** Git 2.x+
- **Fix:**
  ```bash
  sudo apt-get update && sudo apt-get install -y git
  ```

### 3.1 SSH key access (for private repos)

If you use SSH-based git URLs (`git@github.com:...`):

```bash
ssh -T git@github.com 2>&1 | head -1
```

- **Expected:** `Hi <username>! You've successfully authenticated...`
- **Fix:** Generate and add an SSH key:
  ```bash
  ssh-keygen -t ed25519 -C "your_email@example.com"
  eval "$(ssh-agent -s)"
  ssh-add ~/.ssh/id_ed25519
  ```
  Then add the public key to your GitHub account.

### 3.2 SSH agent forwarding

Fleet-man relays the SSH agent the fleet daemon was started with into
containers so git works inside instances. If your agent started after the
daemon, restart it from the TUI (Settings → Fleet Daemon → Restart daemon) so it
picks the agent up. For a remote fleet, also turn on `[ agent: on ]` on its row
in Settings → Fleet Armada (right arrow, enter). The remote only has your agent
while the TUI (or a `fleet up`/`clone`/`rebuild`/`shell` against it) stays
connected.

```bash
echo "SSH_AUTH_SOCK=${SSH_AUTH_SOCK:-not set}"
if [ -n "$SSH_AUTH_SOCK" ]; then
  if [ -S "$SSH_AUTH_SOCK" ]; then
    echo "OK: valid socket"
  else
    echo "FAIL: SSH_AUTH_SOCK is set but is not a valid socket"
  fi
fi
```

- **Fix:** Start the SSH agent:
  ```bash
  eval "$(ssh-agent -s)"
  ssh-add
  ```
  Ensure your shell profile starts the agent automatically.

---

## 4. Node.js and npm

Required to install the devcontainer CLI.

```bash
command -v node && node --version
command -v npm && npm --version
```

- **Expected:** Node.js 18+ and npm
- **Fix:**
  ```bash
  curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
  sudo apt-get install -y nodejs
  ```

---

## 5. Devcontainer CLI

The devcontainer CLI is **required** for local container management.

```bash
command -v devcontainer && devcontainer --version
```

- **Expected:** A version string like `0.x.x`
- **Fix:**
  ```bash
  npm install -g @devcontainers/cli
  ```

### 5.1 Test devcontainer CLI works with Docker

```bash
devcontainer --help > /dev/null 2>&1 && echo "OK" || echo "FAIL"
```

- **Fix:** Ensure Docker is running (see section 2) and the CLI is on your PATH.

---

## 6. Coder CLI (optional)

The Coder CLI enables remote workspace management. It is **optional**.

```bash
command -v coder && coder version
```

- **Fix:** Install from https://coder.com/docs/install

### 6.1 Coder authentication

```bash
coder whoami 2>&1 | head -3
```

- **Expected:** Your Coder username and URL
- **Fix:**
  ```bash
  coder login <your-coder-url>
  ```
  This stores credentials in `~/.config/coderv2/`.

### 6.2 Coder config files

Fleet-man reads Coder credentials from these locations:

```bash
echo "URL file: ${CODER_CONFIG_DIR:-$HOME/.config/coderv2}/url"
echo "Session file: ${CODER_CONFIG_DIR:-$HOME/.config/coderv2}/session"
cat "${CODER_CONFIG_DIR:-$HOME/.config/coderv2}/url" 2>/dev/null || echo "URL file missing"
```

- **Fix:** Run `coder login` or set `CODER_URL` and `CODER_SESSION_TOKEN` environment variables.

---

## 7. tmux (required)

tmux is required — fleet always runs its TUI inside a tmux session. Without tmux, `fleet` will refuse to start.

```bash
command -v tmux && tmux -V
```

- **Fix:**
  ```bash
  sudo apt-get update && sudo apt-get install -y tmux
  ```

---

## 8. Terminal Emulators (legacy fallback)

Fleet always runs inside tmux now, so instance shells open as tmux split panes. The terminal-emulator probe below is only relevant if tmux support has been disabled or removed in a fork.

1. ptyxis
2. gnome-terminal
3. konsole
4. xfce4-terminal
5. alacritty
6. kitty
7. xterm

```bash
for t in ptyxis gnome-terminal konsole xfce4-terminal alacritty kitty xterm; do
  if command -v "$t" > /dev/null 2>&1; then
    echo "FOUND: $t"
  fi
done
```

---

## 9. Fleet-Man Installation

### 9.1 Binary on PATH

```bash
command -v fleet && fleet --help > /dev/null 2>&1 && echo "OK" || echo "FAIL"
```

- **Fix:** Install fleet-man:
  ```bash
  curl -sL https://raw.githubusercontent.com/BenjaminBenetti/fleet-man/main/install.sh | sh
  ```

### 9.2 Fleet directory

Fleet-man stores state in `~/.fleet/`.

```bash
ls -la ~/.fleet/ 2>/dev/null || echo "Directory does not exist (will be created on first run)"
```

Files:
- `~/.fleet/state.json` — fleet and instance state
- `~/.fleet/config.json` — user preferences
- `~/.fleet/workspaces/` — cloned repository workspaces
- `~/.fleet/logs/` — instance creation logs

### 9.3 State file integrity

```bash
if [ -f ~/.fleet/state.json ]; then
  python3 -c "import json; json.load(open('$HOME/.fleet/state.json'))" 2>&1 && echo "OK: valid JSON" || echo "FAIL: corrupt JSON"
else
  echo "No state file (OK for first run)"
fi
```

- **Fix (corrupt state):** Back up and recreate:
  ```bash
  cp ~/.fleet/state.json ~/.fleet/state.json.bak
  echo '{"fleets":{}}' > ~/.fleet/state.json
  ```
  **Warning:** This resets all fleet/instance tracking. Running containers are not affected.

### 9.4 Config file integrity

```bash
if [ -f ~/.fleet/config.json ]; then
  python3 -c "import json; json.load(open('$HOME/.fleet/config.json'))" 2>&1 && echo "OK: valid JSON" || echo "FAIL: corrupt JSON"
else
  echo "No config file (OK, defaults will be used)"
fi
```

- **Fix (corrupt config):**
  ```bash
  cp ~/.fleet/config.json ~/.fleet/config.json.bak
  rm ~/.fleet/config.json
  ```
  Fleet-man will recreate it with defaults on next launch.

---

## 10. Port Availability

Fleet-man binds to local ports for port forwarding. Common conflicts:

```bash
# Check if common dev ports are available
for port in 3000 5000 8000 8080 8443; do
  if ss -tlnp 2>/dev/null | grep -q ":$port "; then
    echo "IN USE: port $port"
    ss -tlnp 2>/dev/null | grep ":$port " | head -1
  else
    echo "FREE: port $port"
  fi
done
```

- **Fix:** Stop the conflicting process or choose a different local port in fleet-man's port forward dialog.

---

## 11. AI Coding Agents (optional)

Fleet-man can detect and display which coding agent is running inside instances. These are all **optional** — do not report missing agents as issues.

```bash
for agent in claude codex auggie gemini copilot; do
  if command -v "$agent" > /dev/null 2>&1; then
    echo "FOUND: $agent"
  fi
done
```

If the user wants to install one:
- **Claude Code:** `npm install -g @anthropic-ai/claude-code`
- **Codex:** `npm install -g @openai/codex` (with node), or the static binary from https://github.com/openai/codex/releases (no node needed; the official install.sh needs gawk — it breaks on mawk-default images)
- **Auggie (Augment Code):** `npm install -g @augmentcode/auggie`
- **Gemini:** See Google's latest CLI install instructions
- **Copilot:** `gh extension install github/gh-copilot`

---

## 12. Clipboard Tools (optional)

Fleet-man's copy/paste support relies on a system clipboard CLI.
**Only one** of `wl-copy` (Wayland) or `xclip` (X11) is required — the
user does **not** need both. Which one applies depends on their display
server:

- Wayland sessions → `wl-copy`
- X11 sessions → `xclip`

This is **optional**. If neither is present, do not report it as an
issue unless the user asks about copy/paste or has already chosen a
display server that implies one. If at least one is installed, copy/paste
is satisfied.

```bash
have_wl=0; have_xclip=0
command -v wl-copy > /dev/null 2>&1 && have_wl=1
command -v xclip  > /dev/null 2>&1 && have_xclip=1

if [ "$have_wl" = "1" ] || [ "$have_xclip" = "1" ]; then
  echo "OK: clipboard tool present (wl-copy=$have_wl, xclip=$have_xclip)"
else
  echo "MISSING: neither wl-copy nor xclip installed"
  echo "Detected display server: ${XDG_SESSION_TYPE:-unknown} (WAYLAND_DISPLAY=${WAYLAND_DISPLAY:-unset}, DISPLAY=${DISPLAY:-unset})"
fi
```

- **Fix (Wayland — install wl-clipboard):**
  ```bash
  sudo apt-get update && sudo apt-get install -y wl-clipboard
  ```
- **Fix (X11 — install xclip):**
  ```bash
  sudo apt-get update && sudo apt-get install -y xclip
  ```

Pick whichever matches `XDG_SESSION_TYPE` / the presence of
`WAYLAND_DISPLAY` vs `DISPLAY`. Do not install both.

---

## 13. xdg-open (optional)

The TUI settings page can open install URLs in your browser. This is **optional** — do not report it missing as an issue.

```bash
command -v xdg-open && echo "OK"
```

If installed but broken, reinstall with:
  ```bash
  sudo apt-get install -y xdg-utils
  ```

---

## 14. Devcontainer Project Requirements

For a repository to work with fleet-man's devcontainer backend, it needs a `.devcontainer/devcontainer.json` file.

When diagnosing a specific project, check:

```bash
# Check current directory for devcontainer config
if [ -f .devcontainer/devcontainer.json ]; then
  echo "OK: devcontainer.json found"
  cat .devcontainer/devcontainer.json
else
  echo "MISSING: No .devcontainer/devcontainer.json in current directory"
  echo "Create one to use fleet-man with this project"
fi
```

---

## 15. Workspace Directory Permissions

```bash
# Check fleet workspaces directory
ws_dir="$HOME/.fleet/workspaces"
if [ -d "$ws_dir" ]; then
  echo "Workspace dir exists: $ws_dir"
  ls -la "$ws_dir" | head -5
  # Check writability
  if [ -w "$ws_dir" ]; then
    echo "OK: writable"
  else
    echo "FAIL: not writable"
  fi
else
  echo "Workspace dir does not exist yet (will be created on first instance)"
fi
```

- **Fix:**
  ```bash
  sudo chown -R $USER:$USER ~/.fleet/
  chmod -R u+rwX ~/.fleet/
  ```

---

## 16. Orphaned Containers

If fleet-man crashes or state becomes inconsistent, Docker containers may be left running without fleet-man tracking them.

```bash
# List containers with devcontainer labels that might be fleet-man instances
docker ps -a --filter "label=devcontainer.local_folder" --format "table {{.ID}}\t{{.Names}}\t{{.Status}}" 2>/dev/null
```

- **Fix:** To clean up orphaned containers:
  ```bash
  # List and optionally remove stopped fleet containers
  docker ps -a --filter "label=devcontainer.local_folder" --filter "status=exited" -q | xargs -r docker rm
  ```

---

## 17. Kubernetes Clusters (optional)

Fleet can run instances as pods on Kubernetes clusters. This section is
**optional**: skip it unless the user runs instances on a cluster, i.e. `fleet
cluster ls` lists a connection. Never report "no cluster connected" as an
issue. Everything here runs **on the daemon host** (where fleetd runs); for a
remote fleet, the `fleet` commands below already answer from there, but
`kubectl` and `docker` checks must run on that host. The reference is
[Fleet on Kubernetes](https://github.com/BenjaminBenetti/fleet-man/blob/main/doc/kubernetes.md).

### 17.1 Connections and their health

```bash
fleet cluster ls
```

- **Expected:** every connection `ready` or `limited` (usable; `fleet cluster
  ls <name>` prints the warnings that make it limited).
- **Otherwise** run the checks, which print a fix under each failing one:
  ```bash
  fleet cluster test <name>          # exit 0 when ready or limited
  fleet cluster test --deep <name>   # also throwaway-pod probes (minutes)
  ```

| State | Fix |
|---|---|
| `unreachable` | the API server does not answer from the daemon host: VPN, the cluster being up, the kubeconfig's server URL |
| `unauthorized` | the login expired: `fleet cluster login <name>` (relays the device code), or TUI Settings → Kubernetes → [ login on fleetd ] |
| `plugin missing` | install the context's exec plugin on the daemon host, or add its folder: `fleet cluster set <name> extra_path=/path/to/bin` |
| `context missing` | the context left the pinned kubeconfig: restore it, or re-point the connection (the cluster editor's Context row) |
| `forbidden` | grant the verbs the `rbac` check lists (see 17.3) |
| `misconfigured` | the check names the setting or namespace to fix |
| `owner conflict` | another fleet daemon holds this connection (a copied `~/.fleet`); stop it, or wait for its Lease to expire |

### 17.2 Build tools on the daemon host

Cluster images are built where fleetd runs, so cluster targets need Docker
(section 2) and the devcontainer CLI (section 5) **on the daemon host**, even
when the user's own machine has neither. A missing one shows as `docker not
found where fleetd runs` on every cluster Deploy option, and in TUI Settings →
Tool Status as `on fleetd @ <host>`.

### 17.3 Kubeconfig, namespace and permissions

```bash
fleet cluster contexts                                 # what fleetd's kubeconfig holds
kubectl --context <ctx> get namespace <ns>             # the namespace exists
kubectl --context <ctx> -n <ns> auth can-i create pods/exec
kubectl --context <ctx> -n <ns> auth can-i --list      # everything fleet's identity may do
```

- **Namespace missing:** fleet never creates one unasked. Create it, or let
  `fleet cluster add <ctx> --create-namespace` (or the cluster editor's
  `[ Create ]`) do it, which needs `namespaces create`.
- **RBAC:** the `rbac` check names each missing verb. The full Role is in
  [What you need](https://github.com/BenjaminBenetti/fleet-man/blob/main/doc/kubernetes.md#2-what-you-need).
  Ask before applying RBAC changes: they are the cluster owner's call.

### 17.4 Admission: root, privileged pods, debug capabilities

`fleet cluster test <name>` reports the namespace's admission:

- `admission_unprivileged` warns `root containers denied`: the namespace
  (pod-security `restricted`) runs every container as one non-root user, with
  no apt or sudo. Fix: a namespace at the `baseline` level or looser.
- `admission_privileged` warns `Allow privileged pods is off`: Docker-in-Docker,
  the docker.sock sidecar and `--device` are refused. Fix, if the user accepts
  that the node becomes the isolation boundary: `fleet cluster set <name>
  allow_privileged=true` (ideally with a dedicated node pool,
  `node_selector=` / `tolerations=`), or a sysbox runtime class
  (`runtime_class=`).
- `admission_debug_caps` warns that SYS_PTRACE / unconfined seccomp are denied:
  they are dropped with a warning (`debug_capabilities=require` fails instead).

### 17.5 Storage

- `root_storage` and `shared_storage` name the StorageClasses Auto picked and
  why. A disk that never binds fails the create `kubernetes:pvc_pending`:
  `kubectl -n <ns> describe pvc <name>` says why.
- `fleet cluster test --storage <class> <name>` runs the multi-node share test
  on a class now. A limited `each fleet on one node` means no class passed: each
  fleet's instances share one node (a fleet-size cap applies).

### 17.6 Image delivery

- `image_delivery` says how built images reach the nodes (node load on a local
  cluster, a registry, or the Fleet gateway) and why not.
- **Registry:** the daemon host's `docker login` for that registry must work;
  an insecure registry also needs the host Docker's `insecure-registries` and
  the nodes' containerd config.
- **Gateway:** TUI Settings → Fleet MCP → **Enable Image Registry**, and a
  gateway run with `--session-key` and an `https` `--public-url`;
  `fleet cluster test --deep <name>` has a pod pull through it (`image_pull`).
- **`ErrImageNeverPull`** on a node-loaded image: the kubelet once pulled the
  same image under another name and now checks pull credentials. Use registry
  delivery for that cluster, or set the kubelet's
  `imagePullCredentialsVerificationPolicy` to `NeverVerify`.

### 17.7 A failed or parked instance

In the TUI, `enter` on the instance says what happened and links the setting
that fixes it; the MCP `fleet_list` record carries the same `error`. From a
shell:

```bash
fleet ls                                               # its status (failed, stopped)
cat ~/.fleet/logs/<fleet>-<instance>.log               # the creation log (on the daemon host)
fleet logs <fleet>/<instance>                          # the dev container's output
kubectl --context <ctx> -n <ns> describe pod <sts>-0   # <sts> is in the container id: k8s:<cid>/<ns>/<sts>@g<n>
```

| Error code | Fix |
|---|---|
| `kubernetes:unsupported:compose` | compose devcontainers do not run on clusters: use Docker for this fleet |
| `kubernetes:unsupported:privileged`, `…:dood-needs-privileged` | see 17.4 |
| `kubernetes:admission_denied` | a namespace whose admission allows the pod, or change the devcontainer |
| `kubernetes:quota` | room in the namespace's ResourceQuota, or the connection's `max_instances` / `max_total_disk` |
| `kubernetes:unschedulable` | a node that fits the pod, or smaller `resources.cpu_request` / `resources.memory_request` |
| `kubernetes:image_pull` | see 17.6 |
| `kubernetes:oomkilled` | raise `memory_limit`, then start the instance again |
| `kubernetes:crash_loop` | `fleet logs` shows why it exits; start it again once fixed |
| `kubernetes:timeout` | the cluster was slow: raise the wait's `FLEET_KUBE_*_TIMEOUT` where fleetd starts |
| `kubernetes:interrupted`, `kubernetes:rebuild_failed` | `fleet rebuild <fleet>/<instance>` (or `R`) retries it |
| `kubernetes:tampered` | the pod or StatefulSet was changed outside fleet: destroy and recreate the instance |
| `kubernetes:cluster_missing` | the connection was removed: reconnect it, or destroy the instance |

### 17.8 Shared folders

On a cluster, agent logins, `~/.claude.json` and custom mounts are synced
between the daemon host and the cluster:

```bash
fleet sync status            # each fleet's folders, what is pending, the shares
fleet sync wait <fleet>      # exits 1 naming what is behind
fleet sync conflicts         # conflict copies kept on the daemon host
```

A row badge `sync paused: <reason>` names the cause (a size cap, a full share,
the daemon host under 5% free). Each cluster logs in to the agents once per
fleet: an agent asking to log in on a cluster instance is expected the first
time.

### 17.9 Leftovers in the cluster

```bash
fleet cluster gc <name>      # dry run: what no instance record accounts for
```

- **Fix (only with the user's consent):** `fleet cluster gc <name> --apply`
  deletes what the listing marks. Mounted volumes, objects younger than an
  hour and those of existing fleets are kept.

---

## Summary Template

After running all checks, report your findings using this format:

```
Fleet-Man Doctor Report
========================

Required:
  Platform:      [Linux / other]
  Docker:        [OK / ISSUE: description]
  Git:           [OK / ISSUE: description]
  Node/npm:      [OK / ISSUE: description]
  devcontainer:  [OK / ISSUE: description]
  Fleet dir:     [OK / ISSUE: description]
  State file:    [OK / CORRUPT / NOT YET CREATED]
  Config file:   [OK / CORRUPT / NOT YET CREATED]

Optional (installed):
  [only list optional tools that ARE installed, e.g.:]
  coder:         [OK / AUTH ISSUE]
  tmux:          OK
  Agents:        claude, codex
  Kubernetes:    [only if a cluster is connected: <name> ready / limited (why) / ISSUE]

Issues Fixed:
- [list each issue you fixed — never list missing optional tools]

Remaining Issues:
- [list issues requiring manual intervention — never list missing optional tools]
```
