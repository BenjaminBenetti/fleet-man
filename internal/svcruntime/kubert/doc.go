// Package kubert is the shared-service runtime of one Kubernetes connection
// (svcruntime.ForCluster): what a fleet's pods have in the connection's
// namespace besides themselves, and the fleet's shared services there.
//
// A fleet on a connection has, all named by its h8 (names.FleetObject):
//
//   - an anchor ConfigMap, the owner every other object below names, so
//     deleting it (fleet's own teardown, or `fleet cluster gc --apply`)
//     takes them all with it;
//   - a headless Service, the StatefulSets' serviceName, which gives each
//     instance's pod <sts>-0 a DNS name its siblings resolve
//     (<sts>-0.<service>, exported in fleet.rc as FLEET_PEER_DOMAIN);
//   - three NetworkPolicies: instances take connections only from the
//     fleet's own instances, the caches only from the fleet's instances on
//     their ports, and the fleet's pods reach anything but the cloud
//     metadata range (169.254.0.0/16, fd00:ec2::/64), DNS excepted, while
//     the connection blocks metadata (its block_metadata, on by default).
//     Labels are segmentation, not authentication: anyone who can label a
//     pod in the namespace reaches the caches;
//   - for each shared service the fleet has on, a volume (kept when the
//     fleet leaves, so the next one starts warm), a Deployment (Recreate)
//     and a ClusterIP Service, on digest-pinned images, each cleaning up
//     below its volume's size (buildkit's GC, the registry mirror's 7-day
//     expiry of proxied blobs, apt-cacher-ng's expiry run the daemon starts
//     once a day). A nearly full volume grows where its StorageClass allows,
//     else its Caching row warns.
//
// Every workload goes through podspec.Harden. Objects are server-side
// applied under fleet's field manager, never over an object of the same name
// that is not this daemon's (the owner, connection and fleet labels say
// whose it is). Each fleet's work on a connection is serialized in-process.
//
// When the last instance of a fleet on a connection goes (ReleaseFleet) or
// the fleet does (TeardownFleet), its objects there are deleted (the caches'
// volumes too, on a fleet destroy, where the connection deletes them with
// the fleet). A teardown the cluster cannot take now is recorded in
// ~/.fleet/kube/footprint.json and finished by the reconciler once the
// connection is usable again (RunPendingTeardowns).
//
// Everything that needs client-go builds only in the full binary (!nok8s);
// this file builds everywhere, so the package always exists.
package kubert
