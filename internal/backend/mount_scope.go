package backend

import "fmt"

// MountScope classifies where a Mount's source lives, which decides how a
// backend provides it. Docker-based backends bind LocalPath whatever the
// scope; a backend with its own storage maps each scope onto that storage.
type MountScope uint8

const (
	// MountScopeHost is a path on the daemon host, bound as is. Only a
	// backend that shares the daemon host's filesystem can honor it. It is
	// the zero value, so a Mount built without a scope keeps this meaning.
	MountScopeHost MountScope = iota
	// MountScopeFleetShared is state every instance of a fleet shares, kept
	// under the fleet's mount root (~/.fleet/workspaces/<fleet>, the tree the
	// mounts resolver creates). SubPath is relative to that root.
	MountScopeFleetShared
)

// String returns the scope's name for logs and error messages.
func (s MountScope) String() string {
	switch s {
	case MountScopeHost:
		return "host"
	case MountScopeFleetShared:
		return "fleet-shared"
	default:
		return fmt.Sprintf("MountScope(%d)", uint8(s))
	}
}
