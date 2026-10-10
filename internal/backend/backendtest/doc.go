// Package backendtest is the backend conformance suite: the contract every
// backend.Backend keeps, written once as cases and run against each real
// backend (devcontainer now, Kubernetes once it lands) and against Stub, the
// in-memory backend that unit tests use.
//
// Method cases check one Backend method, or a small group of them, on one
// instance the method cases share. Scenario cases drive a lifecycle (stop and
// start, rebuild, clone, a failed create) on instances of their own. Every
// instance the suite creates is torn down the way the daemon tears one down
// (Down by ContainerID, else OrphanDowner.DownByKey), and the backend must then
// hold nothing for it: Target.Leftovers comes back empty.
//
// Instances are created from the conformance fixture (fixture.go): a
// Dockerfile devcontainer on mcr.microsoft.com/devcontainers/base:debian-12
// with tmux and socat, whose postCreateCommand and postStartCommand count
// themselves into the instance's $HOME.
//
// Real backends run only when FLEET_CONFORMANCE selects them, from the test-only
// package internal/backend/conformance:
//
//	FLEET_CONFORMANCE=devcontainer go test -timeout 60m ./internal/backend/conformance
//
// The value is a comma-separated list of backend[:qualifier] entries; the
// qualifier names the kube context for kubernetes
// (FLEET_CONFORMANCE=devcontainer,kubernetes:minikube). Without the variable
// those tests skip, so go test ./... stays fast and needs neither docker nor a
// cluster. This package's own tests run the whole suite against Stub on every
// go test, and run each case against a deliberately broken Stub to prove the
// case notices.
package backendtest
