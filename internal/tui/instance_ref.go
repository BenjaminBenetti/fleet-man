package tui

import (
	"strings"
)

// InstanceRef uniquely identifies an instance across all fleets. Every
// session-state operation requires one so two instances that share a
// name (or a session/group name) can never alias each other.
type InstanceRef struct {
	Fleet    string
	Instance string
}

// Valid reports whether both halves of the reference are populated.
// SessionStore methods silently no-op on invalid refs.
func (r InstanceRef) Valid() bool {
	return r.Fleet != "" && r.Instance != ""
}

// Key returns the legacy "fleet/instance" string form. Reserve for
// presentation (status messages, log lines) and external persistence
// keys; inside fleet, pass the typed ref instead.
func (r InstanceRef) Key() string {
	return r.Fleet + "/" + r.Instance
}

// ParseInstanceRef parses a "fleet/instance" string back into a typed
// ref. Returns ok=false when the input is malformed.
func ParseInstanceRef(key string) (InstanceRef, bool) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return InstanceRef{}, false
	}
	return InstanceRef{Fleet: parts[0], Instance: parts[1]}, true
}
