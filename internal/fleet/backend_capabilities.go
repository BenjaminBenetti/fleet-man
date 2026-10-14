package fleet

// BackendCapabilities are the actions a backend type supports on an existing
// instance. They are hints for code that cannot construct a backend, such as
// the TUI, which gates its clone (C) and rebuild (R) keys on them. The server
// stays authoritative: its jobs ask the live backend (SupportsClone,
// SupportsRebuild) and refuse what it cannot do. backendutil's tests keep this
// table equal to what each backend reports.
type BackendCapabilities struct {
	// Clone: the backend can snapshot an instance into a new one.
	Clone bool
	// Rebuild: the backend can recreate an instance's container in place,
	// keeping its workspace.
	Rebuild bool
}

// backendCapabilities holds one row per BackendType this build knows.
var backendCapabilities = map[BackendType]BackendCapabilities{
	BackendDevcontainer: {Clone: true, Rebuild: true},
	BackendCodespaces:   {Rebuild: true},
	BackendCoder:        {},
	// A cluster instance rebuilds by reseeding its disk (which is also how a
	// failed create is retried) and clones through a snapshot, a CSI clone
	// or a copier Job.
	BackendKubernetes: {Clone: true, Rebuild: true},
}

// allCapabilities is what a type without a row reports.
var allCapabilities = BackendCapabilities{Clone: true, Rebuild: true}

// Capabilities returns the backend type's capability hints. "" is the legacy
// spelling of devcontainer. A type this build has no row for reports every
// capability, so the caller leaves the decision to the server instead of
// refusing an action the backend may support.
func (t BackendType) Capabilities() BackendCapabilities {
	if t == "" {
		t = BackendDevcontainer
	}
	if capabilities, ok := backendCapabilities[t]; ok {
		return capabilities
	}
	return allCapabilities
}
