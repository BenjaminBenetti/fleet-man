# fleet-man integration fixture — no devcontainer

A bare repository deliberately missing `.devcontainer/devcontainer.json`,
used by the new-fleet devcontainer-check tests to exercise the "no
devcontainer" dialog (Abort / Setup branches). The check-flow tests
copy this directory into a throwaway git repo and feed its `file://`
URL into the TUI's new-fleet dialog.

Keeping it disjoint from the default fixture means the present-fixture
happy path and the missing-fixture dialog path can each ship a focused
test without one mutating the other's repo.
