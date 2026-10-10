// Package conformance runs the backend conformance suite
// (internal/backend/backendtest) against the real backends FLEET_CONFORMANCE
// selects. It holds only tests:
//
//	FLEET_CONFORMANCE=devcontainer go test -count=1 -timeout 60m ./internal/backend/conformance
//
// Each selected backend runs as a subtest of TestConformance named after its
// entry (TestConformance/devcontainer/method/exec-tar). A backend this build
// has no target for fails instead of being skipped. Without the variable the
// test skips at once and needs neither docker nor a cluster.
//
// The test process gets a HOME of its own, so the backends' event log
// (~/.fleet/fleet.log) and anything else they keep under HOME stay out of the
// developer's; docker keeps its client config through DOCKER_CONFIG.
//
// devcontainer needs docker and the devcontainer CLI, and pulls
// backendtest.FixtureImage on first use.
package conformance
