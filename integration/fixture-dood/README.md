# fleet-man integration fixture: docker-outside-of-docker

A devcontainer with the docker-outside-of-docker feature: it binds the docker
engine's socket into the instance. On a cluster the engine is the pod's own
`fleet-docker` sidecar.
