// Package fleetpaths holds the well-known filesystem paths the fleet client and
// server agree on (the ~/.fleet directory, the server socket, the lock files,
// the version-hint file).
//
// It is deliberately a leaf, dependency-free package: the CLIENT side
// (internal/fleetclient) needs the socket/lock paths to dial and auto-spawn the
// server, but the client must NOT import internal/state (a server-only package,
// per the architecture boundary). So these few paths live here, importable by
// both sides, rather than in internal/state.
package fleetpaths

import (
	"os"
	"path/filepath"
)

// Dir is the per-user fleet directory (~/.fleet) that the server owns.
func Dir() string {
	return filepath.Join(os.Getenv("HOME"), ".fleet")
}

// SocketPath is the unix domain socket the fleet server listens on and clients
// dial. Host-local and per-user.
func SocketPath() string {
	return filepath.Join(Dir(), "fleet.sock")
}

// ServerLockPath is the file the running server holds an exclusive flock on for
// its entire lifetime, guaranteeing at most one server per user.
func ServerLockPath() string {
	return filepath.Join(Dir(), "server.lock")
}

// SpawnLockPath is the file clients flock briefly while spawning (or restarting)
// the server, so N racing clients produce exactly one spawn.
func SpawnLockPath() string {
	return filepath.Join(Dir(), "spawn.lock")
}

// VersionFilePath is a cheap pre-dial hint the server writes on startup. It is
// NEVER authoritative — the Hello handshake is. Clients may read it to avoid a
// dial, but must still Hello.
func VersionFilePath() string {
	return filepath.Join(Dir(), "server.version")
}

// WorkspacesDir is the base directory for instance workspace clones. Mirrors
// internal/state.WorkspacesDir; lives here too so client code (which must not
// import internal/state) can derive workspace paths.
func WorkspacesDir() string {
	return filepath.Join(Dir(), "workspaces")
}

// WarnPath is the host-side warning file for a single instance (the TUI banners
// its first line after creation). Mirrors internal/state.WarnPath.
func WarnPath(fleetName, instanceName string) string {
	return filepath.Join(Dir(), "logs", fleetName+"-"+instanceName+".warn")
}
