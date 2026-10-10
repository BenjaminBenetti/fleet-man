# fleet-man docker-in-docker fixture

Adds the published docker-in-docker feature: a privileged container running
its own dockerd, with /var/lib/docker on a per-instance named volume
(`dind-var-lib-docker-${devcontainerId}`).
