# fleet-man Dockerfile + features fixture

Builds its image from `.devcontainer/Dockerfile` with the repo root as the
build context and a build arg, adds the local feature
`.devcontainer/itest-feature` with an option, and sets containerEnv and
remoteEnv. The Dockerfile copies this file into the image.
