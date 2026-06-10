package admiralmcp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withHome points HOME (and USERPROFILE on Windows) at a temp dir so
// EnsureInstalled reads an isolated ~/.fleet and writes an isolated
// ~/.claude.json, never touching the real user home.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Endpoint selection must look local regardless of the developer's shell.
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	return home
}

// publishEndpoint writes the daemon's MCP discovery files (~/.fleet/mcp.port,
// mcp.token) the way the server does on startup.
func publishEndpoint(t *testing.T, home, port, token string) {
	t.Helper()
	dir := filepath.Join(home, ".fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.fleet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.port"), []byte(port), 0o600); err != nil {
		t.Fatalf("write mcp.port: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.token"), []byte(token), 0o600); err != nil {
		t.Fatalf("write mcp.token: %v", err)
	}
}

// readConfigTree parses the installed ~/.claude.json for assertions.
func readConfigTree(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, configFile))
	if err != nil {
		t.Fatalf("read %s: %v", configFile, err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", configFile, err)
	}
	return root
}

// fleetEntry digs mcpServers.fleet out of the parsed config.
func fleetEntry(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	servers, ok := root[mcpServersKey].(map[string]any)
	if !ok {
		t.Fatalf("config has no %q object: %v", mcpServersKey, root)
	}
	entry, ok := servers[serverKey].(map[string]any)
	if !ok {
		t.Fatalf("config has no %q server entry: %v", serverKey, servers)
	}
	return entry
}

// TestEnsureInstalledNotReadyWithoutEndpoint verifies the retryable ErrNotReady
// path while the daemon hasn't published its discovery files, and that no
// config file is created in that state.
func TestEnsureInstalledNotReadyWithoutEndpoint(t *testing.T) {
	home := withHome(t)

	if err := EnsureInstalled(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("EnsureInstalled() with no ~/.fleet = %v, want ErrNotReady", err)
	}

	// Port alone is not enough: the token gates access.
	publishEndpoint(t, home, "6012", "tok")
	if err := os.Remove(filepath.Join(home, ".fleet", "mcp.token")); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	if err := EnsureInstalled(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("EnsureInstalled() without token = %v, want ErrNotReady", err)
	}

	if _, err := os.Stat(filepath.Join(home, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was created on the not-ready path", configFile)
	}
}

// TestEnsureInstalledRejectsGarbledPort verifies a present-but-invalid port
// file is a real (non-retryable) error, while an EMPTY port file — a reader
// catching the daemon's truncate-then-write mid-flight — stays retryable.
func TestEnsureInstalledRejectsGarbledPort(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "not-a-port", "tok")

	err := EnsureInstalled()
	if err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("EnsureInstalled() with garbled port = %v, want non-retryable error", err)
	}

	publishEndpoint(t, home, "", "tok")
	if err := EnsureInstalled(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("EnsureInstalled() with empty port file = %v, want ErrNotReady", err)
	}
}

// TestEnsureInstalledFailsFastOnUnreadablePort verifies a read failure that
// is NOT absence (here: the port path is a directory) is a non-retryable
// error — only "file doesn't exist yet" means the daemon isn't up.
func TestEnsureInstalledFailsFastOnUnreadablePort(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	portPath := filepath.Join(home, ".fleet", "mcp.port")
	if err := os.Remove(portPath); err != nil {
		t.Fatalf("remove port file: %v", err)
	}
	if err := os.Mkdir(portPath, 0o700); err != nil {
		t.Fatalf("mkdir in place of port file: %v", err)
	}

	err := EnsureInstalled()
	if err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("EnsureInstalled() with unreadable port = %v, want non-retryable error", err)
	}
}

// TestEnsureInstalledCreatesConfig verifies a fresh install creates
// ~/.claude.json (0600 — it embeds the bearer token) with the fleet server
// entry pointing at the published endpoint.
func TestEnsureInstalledCreatesConfig(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6017\n", "secret-token\n")

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() = %v, want nil", err)
	}

	entry := fleetEntry(t, readConfigTree(t, home))
	if entry["type"] != "http" {
		t.Errorf("type = %v, want http", entry["type"])
	}
	if entry["url"] != "http://127.0.0.1:6017" {
		t.Errorf("url = %v, want http://127.0.0.1:6017", entry["url"])
	}
	headers, _ := entry["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer secret-token" {
		t.Errorf("Authorization = %v, want Bearer secret-token", headers["Authorization"])
	}

	info, err := os.Stat(filepath.Join(home, configFile))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new config mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestEnsureInstalledPreservesExistingConfig verifies the merge keeps every
// unrelated key — including other MCP servers and integers too large for a
// float64 round-trip — and the existing file mode.
func TestEnsureInstalledPreservesExistingConfig(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	// 9007199254740993 = 2^53 + 1: corrupted if decoded into a float64.
	existing := `{
  "numStartups": 9007199254740993,
  "projects": {"/workspaces/app": {"allowedTools": []}},
  "mcpServers": {"other": {"type": "stdio", "command": "other-server"}}
}`
	path := filepath.Join(home, configFile)
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() = %v, want nil", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Errorf("large integer was corrupted on rewrite:\n%s", data)
	}

	root := readConfigTree(t, home)
	if _, ok := root["projects"].(map[string]any); !ok {
		t.Errorf("projects key lost on rewrite: %v", root)
	}
	servers := root[mcpServersKey].(map[string]any)
	if _, ok := servers["other"].(map[string]any); !ok {
		t.Errorf("unrelated MCP server lost on rewrite: %v", servers)
	}
	fleetEntry(t, root) // fleet entry added alongside

	// A permissive pre-existing mode is deliberately clamped to owner-only:
	// the rewrite embeds the MCP bearer token, which is the MCP access
	// boundary and must never be group/world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("rewrite mode = %v, want 0600 (clamped from 0644)", info.Mode().Perm())
	}
}

// TestEnsureInstalledTreatsNullConfigAsEmpty verifies a ~/.claude.json
// containing the JSON literal `null` (which decodes into a nil map without an
// error) installs cleanly instead of panicking on assignment into a nil map —
// the install runs in a TUI goroutine, so a panic here would kill the TUI.
func TestEnsureInstalledTreatsNullConfigAsEmpty(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	path := filepath.Join(home, configFile)
	if err := os.WriteFile(path, []byte("null\n"), 0o600); err != nil {
		t.Fatalf("seed null config: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() on null config = %v, want nil", err)
	}
	fleetEntry(t, readConfigTree(t, home))
}

// TestEnsureInstalledRefusesTrailingGarbage verifies a file whose first JSON
// document parses but is followed by junk is rejected and left untouched —
// it is not the single-document file we know how to merge into.
func TestEnsureInstalledRefusesTrailingGarbage(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	path := filepath.Join(home, configFile)
	corrupt := []byte("{\"keep\": true}\ngarbage")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := EnsureInstalled(); err == nil {
		t.Fatalf("EnsureInstalled() with trailing garbage = nil, want error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("config with trailing garbage was modified: %q", got)
	}
}

// TestEnsureInstalledFollowsSymlinkedConfig verifies a symlinked
// ~/.claude.json (fleet's own Claude-config mount in instances, dotfile
// managers) keeps the link intact and updates the file behind it.
func TestEnsureInstalledFollowsSymlinkedConfig(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	target := filepath.Join(home, "real-claude.json")
	if err := os.WriteFile(target, []byte(`{"projects": {}}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(home, configFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() through symlink = %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("~/%s is no longer a symlink — the link was severed", configFile)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), `"fleet"`) {
		t.Errorf("symlink target was not updated:\n%s", data)
	}
}

// TestEnsureInstalledIsIdempotent verifies the no-op path: when the installed
// entry already matches the live endpoint, the config is not rewritten.
func TestEnsureInstalledIsIdempotent(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("first EnsureInstalled() = %v", err)
	}
	path := filepath.Join(home, configFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("second EnsureInstalled() = %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config after second run: %v", err)
	}
	if !info2.ModTime().Equal(info.ModTime()) {
		t.Errorf("config was rewritten on the no-op path (mtime changed)")
	}
}

// TestEnsureInstalledRefreshesStaleEntry verifies a daemon restart that lands
// on a different port updates the registered URL.
func TestEnsureInstalledRefreshesStaleEntry(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() = %v", err)
	}

	publishEndpoint(t, home, "6013", "tok")
	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() after port change = %v", err)
	}

	entry := fleetEntry(t, readConfigTree(t, home))
	if entry["url"] != "http://127.0.0.1:6013" {
		t.Errorf("url = %v, want refreshed http://127.0.0.1:6013", entry["url"])
	}
}

// TestEnsureInstalledRefusesUnparseableConfig verifies a corrupt
// ~/.claude.json is reported and left byte-for-byte untouched — never
// clobbered.
func TestEnsureInstalledRefusesUnparseableConfig(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	path := filepath.Join(home, configFile)
	corrupt := []byte(`{"mcpServers": {`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt config: %v", err)
	}

	if err := EnsureInstalled(); err == nil {
		t.Fatalf("EnsureInstalled() on corrupt config = nil, want error")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("corrupt config was modified: %q", got)
	}
}

// TestEnsureInstalledRefusesNonObjectMcpServers verifies a malformed (but
// parseable) mcpServers value is an error, not silently replaced.
func TestEnsureInstalledRefusesNonObjectMcpServers(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")

	path := filepath.Join(home, configFile)
	if err := os.WriteFile(path, []byte(`{"mcpServers": []}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := EnsureInstalled(); err == nil {
		t.Fatalf("EnsureInstalled() with non-object mcpServers = nil, want error")
	}
}

// TestEnsureInstalledEventuallySkipsRemote verifies the remote-daemon guard:
// with FLEET_GATEWAY set, nothing is installed even though local discovery
// files exist (they describe a daemon the user isn't talking to).
func TestEnsureInstalledEventuallySkipsRemote(t *testing.T) {
	home := withHome(t)
	publishEndpoint(t, home, "6012", "tok")
	t.Setenv("FLEET_GATEWAY", "https://gw.example:50051/abc")

	if err := EnsureInstalledEventually(time.Second); err != nil {
		t.Fatalf("EnsureInstalledEventually() remote = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(home, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was created for a remote daemon", configFile)
	}
}

// TestEnsureInstalledEventuallyWaitsForEndpoint verifies the cold-start path:
// the endpoint files appearing mid-wait still get installed.
func TestEnsureInstalledEventuallyWaitsForEndpoint(t *testing.T) {
	home := withHome(t)

	// Publish the endpoint files mid-wait, without t.Fatalf (which must not be
	// called from a non-test goroutine); a failed write just times the test out.
	go func() {
		time.Sleep(100 * time.Millisecond)
		dir := filepath.Join(home, ".fleet")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "mcp.port"), []byte("6012"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "mcp.token"), []byte("tok"), 0o600)
	}()

	if err := EnsureInstalledEventually(10 * time.Second); err != nil {
		t.Fatalf("EnsureInstalledEventually() = %v, want nil", err)
	}
	fleetEntry(t, readConfigTree(t, home))
}
