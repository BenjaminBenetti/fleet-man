// Package preflight judges a Kubernetes connection: whether fleet can run
// instances through it, how well, and what that exposes.
//
// Full is the streamed preflight a connection gets when it is added, edited
// or tested, and every 30 minutes: credentials and their expiry, reach and
// identity, the namespace and its pod-security and mesh labels, RBAC
// (SelfSubjectRulesReview, then SelfSubjectAccessReview where the rules are
// incomplete), the owner Lease, nodes, admission dry-runs of generic Pod,
// StatefulSet and Job canaries in the unprivileged, debug-capability and
// privileged shapes (and the shared Buildkit pod), StorageClass
// classification, snapshot classes, LimitRange and ResourceQuota headroom,
// the metrics API, runtime classes and a scan of the namespace's RoleBindings
// for other subjects that can reach into instances. Shallow is the cheap
// probe between full runs: reach and the owner Lease.
//
// The result's state follows from what the checks found, not from their
// pass/warn/fail marks: LIMITED is reserved for degraded usability
// (privileged denied, node-local storage, ...), while exposure facts such as
// other RoleBinding subjects are warnings that never make a working
// connection look broken.
//
// Results are the wire's health messages (fleetgrpc.KubeCheck,
// KubeCapabilities): health is computed by the daemon and only ever
// published, like the other server-owned Watch values, so it has no domain
// model to convert from.
//
// Everything that talks to a cluster needs client-go and is left out of the
// -tags nok8s pod build; this file alone builds there.
package preflight
