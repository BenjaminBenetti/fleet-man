# fleet-man

## Kubernetes (minikube)

The devcontainer ships minikube, kubectl, kubectx, kubens and k9s, but the
minikube cluster is **off by default** to save memory. Turn it on before using
any of them:

```bash
.devcontainer/minikube.sh on       # start/resume; returns once the node is Ready
.devcontainer/minikube.sh off      # stop and free its memory; state kept for a fast restart
.devcontainer/minikube.sh status
```

- Once on, `minikube` is the current kubectl context, so kubectl, kubectx, kubens
  and k9s target it with zero setup.
- While off, kubectl fails with `current-context is not set` or connection
  refused. That means "run `on`", not a misconfiguration.
- The first `on` in a fresh container downloads ~1 GB (about a minute); later
  starts take ~10s.
- `on` passes extra flags to `minikube start` (e.g. `--memory=8g --cpus=4`);
  `minikube delete` wipes the cluster.
- Turn it off when you no longer need it.

## Integration tests

Never run `integration/run.sh` or `integration/tests/*.sh` directly here. This
devcontainer is a fleet instance, and the TUI tests bind, unbind and kill on the
tmux server hosting your own session. Run them in a throwaway QA container; the
recipe, and the one for Kubernetes tests (`FLEET_QA_START_MINIKUBE=1`), is in
DEVELOPMENT.md under "Integration tests" and "Dev loop".
