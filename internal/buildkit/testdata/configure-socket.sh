docker buildx rm fleet-shared >/dev/null 2>&1 || true; docker buildx create --name fleet-shared --driver remote 'unix:///run/fleet-buildkit/buildkitd.sock' && docker buildx use fleet-shared
