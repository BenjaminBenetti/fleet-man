// Package syncpolicy is the table of how sharesync keeps each path of a
// fleet's shared folders (~/.fleet/workspaces/<fleet>/...) converged between
// the daemon host and the fleet's share on each cluster.
//
// The table is data only: sharesync implements each policy. A path belongs to
// the deepest sync root containing it (an agent tool's state folder, the
// shared files folder, the custom mounts folder), and the root's rules,
// matched in order, pick its policy; paths no rule matches get the root's
// default. Every folder the resolver mounts needs a root, and every agent
// tool's login file a credential rule, so a new tool's login can never fall
// through to mirror: TestSyncPolicyCoversEveryMount in
// internal/mounts/resolver fails until defaults.go has both.
package syncpolicy

// Policy says how sharesync converges one path across its copies.
type Policy uint8

const (
	// Unset is the zero Policy: no policy was given. The table never holds it.
	Unset Policy = iota
	// Mirror: the newest change to the path wins everywhere, deletes
	// included.
	Mirror
	// Credential: an agent login file. It syncs live within a location (the
	// daemon host, or one cluster) and stays per location across locations
	// (credential_scope=location), because rotating refresh tokens cannot be
	// refreshed safely from two locations at once; a cluster with
	// credential_scope=fleet (experimental) shares the daemon host's. A
	// delete carrying the current revision is a logout and propagates as a
	// tombstone; a stale delete is rejected.
	Credential
	// JSON3: a JSON settings file merged three ways, hook arrays as sets; an
	// empty object ('{}', the seed a new instance writes) never wins over
	// content.
	JSON3
	// TOML3: a TOML config file merged three ways.
	TOML3
	// AppendUnion: an append-only log; the copies converge on the union of
	// their lines.
	AppendUnion
	// LocationLocal: never synced; each location keeps its own (installer
	// downloads, sessions, live databases, sockets, lock files).
	LocationLocal
)

// String returns the policy's name as the plan and status output spell it.
func (p Policy) String() string {
	switch p {
	case Mirror:
		return "mirror"
	case Credential:
		return "credential"
	case JSON3:
		return "json3"
	case TOML3:
		return "toml3"
	case AppendUnion:
		return "append-union"
	case LocationLocal:
		return "location-local"
	}
	return "unset"
}
