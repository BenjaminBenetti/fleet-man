# fleet-man privileged fixture

The base fixture run with `--privileged`, as docker-in-docker instances are:
on a cluster such a pod has fleet's cgroup layout, so a clone can freeze its
user processes for a consistent copy.
