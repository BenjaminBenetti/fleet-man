# fleet-man buildx fixture

The integration fixture's image with the docker CLI and its buildx plugin
copied in from the official `docker:cli` image (static binaries) and no
docker daemon: builds go to the fleet's shared Buildkit server through the
`fleet-shared` remote builder fleet configures, on any backend.
