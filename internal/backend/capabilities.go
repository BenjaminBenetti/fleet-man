package backend

// Capabilities is the pure capability surface of a Backend: questions it
// answers without I/O. backendutil.CapabilitiesFor returns one for callers that
// only ask (a job's precondition check under the state lock, a reconcile
// sweep). Acting on an instance takes the full Backend from
// backendutil.NewForInstance.
type Capabilities interface {
	SupportsSharedMounts() bool
	SupportsClone() bool
	SupportsRebuild() bool
	SupportsMicSink() bool
}
