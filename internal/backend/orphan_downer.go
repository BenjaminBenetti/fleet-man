package backend

// OrphanDowner is an optional capability for backends that can tear an
// instance down when it has no ContainerID. That happens when a create fails,
// or is interrupted, after it has started allocating resources: Kubernetes
// objects exist long before the ContainerID is recorded at Running, so Down,
// which takes a ContainerID, can't reach them.
//
// The handle must come from backendutil.NewForInstance, which binds the
// instance's identity (for Kubernetes, its cluster and object key) into it.
type OrphanDowner interface {
	// DownByKey removes everything the bound instance owns, found by the key it
	// was created under instead of by ContainerID. It returns nil when there is
	// nothing to remove, including when the instance never got a key.
	DownByKey() error
}
