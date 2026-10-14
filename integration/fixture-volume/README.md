# fleet-man named-volume fixture

Mounts a named volume (not one named after `${devcontainerId}`) over
`/etc/skel`, which the image fills, so a new volume is copied up from the
image. Every instance of the fleet naming it shares it, as every container
naming a docker volume does; the last instance destroyed takes it with it.
