# fleet-man host-bind fixture

Declares the two kinds of host bind users write for credentials and config,
the way `~/.aws` (a read-write directory) and `~/.gitconfig` (a read-only
file) are usually bound in. The sources live under
`$FLEET_ITEST_HOSTBIND_DIR` (read from the daemon's environment) so a test
never touches the real `~/.aws` or `~/.gitconfig` on the machine running it.
