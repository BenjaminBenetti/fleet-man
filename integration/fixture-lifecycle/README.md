# fleet-man lifecycle hooks fixture

Every devcontainer lifecycle hook appends its name to `.lifecycle.log` in the
workspace (`initializeCommand`, which runs on the daemon host, to
`initialize.log` beside the checkout), so a test can count how often each one
ran across create, stop/start and rebuild.
