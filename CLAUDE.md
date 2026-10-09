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
