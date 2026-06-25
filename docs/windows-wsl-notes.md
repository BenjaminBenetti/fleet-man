# Windows WSL Notes

These notes capture the Windows/WSL setup used to build and smoke test Fleet.

## Quickstart

Prerequisites:

- WSL2 with Ubuntu installed
- Docker Desktop with WSL integration enabled for the Ubuntu distro
- Git available inside WSL
- Git access to the target repository; SSH is only needed for private repos or
  SSH remotes

Inside WSL:

```bash
# Install/use Node through nvm, not Ubuntu's old distro Node.
nvm install 22
nvm alias default 22
nvm use default
npm install -g @devcontainers/cli

# Fleet/Docker Desktop WSL workaround.
export FLEET_DEVCONTAINER_BUILDKIT=never

# Confirm tooling.
node --version
npm --version
devcontainer --version
docker info
fleet --help
```

## Health Check

Run Fleet from a WSL filesystem path, such as `~/code/fleet-man`, not from
`/mnt/c`, when possible.

```bash
uname -a
pwd
which node && node --version
which npm && npm --version
which devcontainer && devcontainer --version
which docker && docker info
which fleet && fleet --help
```

Condensed check:

```bash
printf 'FLEET_DEVCONTAINER_BUILDKIT=%s\n' "$FLEET_DEVCONTAINER_BUILDKIT"
node --version
npm --version
devcontainer --version
docker info --format 'Docker {{.ServerVersion}} {{.OSType}} running={{.ContainersRunning}} total={{.Containers}}'
fleet --help >/dev/null && echo fleet-ok
fleet ls
```

If the Fleet logo or borders render without the expected colors, confirm the
terminal and tmux session expose 256-color or truecolor support:

```bash
echo "$TERM"
tmux show -g terminal-features 2>/dev/null || true
```

Windows Terminal plus modern tmux should preserve Fleet's truecolor gradient.

## Node and devcontainer CLI

Do not install modern devcontainer CLI packages against Ubuntu's old distro
Node. Use nvm in WSL:

```bash
nvm install 22
nvm alias default 22
nvm use default
npm install -g @devcontainers/cli
```

If an older user-local devcontainer CLI exists under `~/.local`, remove it so
shells do not accidentally run it with an old Node:

```bash
npm uninstall -g --prefix ~/.local @devcontainers/cli
hash -r
```

## BuildKit Workaround

Fleet supports an opt-in BuildKit mode for devcontainer startup:

```bash
export FLEET_DEVCONTAINER_BUILDKIT=never
```

This is useful on Docker Desktop plus WSL when devcontainer startup fails while
building the UID-adjustment image or feature image. Accepted values are:

- `auto`
- `never`

Unset behavior delegates to the devcontainer CLI default.

## Disposable Local Fixture

For a low-risk local smoke test, create a tiny disposable repo:

```bash
mkdir -p ~/code/fleet-disposable/.devcontainer
cd ~/code/fleet-disposable
git init
git config user.name "Fleet Disposable"
git config user.email "fleet-disposable@example.com"

cat > README.md <<'EOF'
# fleet-disposable

Disposable devcontainer fixture for Fleet smoke testing.
EOF

cat > .devcontainer/devcontainer.json <<'EOF'
{
  "name": "fleet-disposable",
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu-22.04",
  "remoteUser": "vscode"
}
EOF

git add README.md .devcontainer/devcontainer.json
git commit -m "Add disposable devcontainer fixture"
```

Run and clean it:

```bash
fleet up disposable-1 --repo ~/code/fleet-disposable
fleet ls
fleet exec fleet-disposable/disposable-1 -- sh -lc 'echo disposable-ok && pwd && id -un'
fleet down fleet-disposable/disposable-1
```

Expected exec output:

```text
disposable-ok
/workspaces/fleet-disposable
vscode
```
