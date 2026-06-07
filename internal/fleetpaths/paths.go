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

// EnsureDir creates ~/.fleet if it does not exist, returning its path. The
// server's Serve does the same on startup, but the CLIENT needs the directory
// to exist BEFORE that — it opens the spawn lock file there to serialize the
// very spawn that would start the server. O_CREATE makes the lock file, not its
// parent, so on a machine that has never run fleet (no ~/.fleet yet) the client
// would otherwise fail with ENOENT before it could spawn the server at all.
// 0700 matches the perms Serve uses.
func EnsureDir() (string, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
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

// McpPortPath records the TCP port the server's MCP (Model Context Protocol)
// HTTP server bound to. Like VersionFilePath it is a non-authoritative discovery
// file the server writes on startup and removes on shutdown, so a user or
// program can find the MCP endpoint when the default port (6012) was taken and
// the server had to fall back to the next free one.
func McpPortPath() string {
	return filepath.Join(Dir(), "mcp.port")
}

// McpTokenPath holds the bearer token a client must send to the MCP HTTP server.
// The TCP port is reachable by any local user, so (unlike the 0600 unix socket)
// the port alone is not an access boundary; the token file is written 0600 so
// only the owning user can read it, restoring per-user access. It is generated
// once and reused across restarts (persistent), so env vars and mcp.json files
// that reference it stay valid.
func McpTokenPath() string {
	return filepath.Join(Dir(), "mcp.token")
}

// McpEnvPath is a sourceable shell snippet (FLEET_MCP_PORT/URL/TOKEN exports)
// the server refreshes on startup. ~/.bashrc sources it so MCP client configs
// (mcp.json) can reference ${FLEET_MCP_URL} / ${FLEET_MCP_TOKEN}. Written 0600
// because it embeds the token.
func McpEnvPath() string {
	return filepath.Join(Dir(), "mcp.env")
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
