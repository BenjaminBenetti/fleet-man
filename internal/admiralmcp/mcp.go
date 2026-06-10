// Package admiralmcp registers the local fleet MCP server in the user's
// Claude Code configuration (~/.claude.json) on client startup. It is the
// companion to internal/admiralskill: the skill teaches an agent HOW to drive
// fleet's MCP tools, this package makes those tools reachable by installing
// the user-scope server entry Claude Code reads at session start.
//
// The entry is merged, never overwritten wholesale: ~/.claude.json is Claude
// Code's primary state file, so the whole document is decoded, only
// mcpServers.fleet is replaced, and the rest is re-encoded untouched (numbers
// kept verbatim via json.Number so a float64 round-trip can't corrupt large
// integers). A file that fails to parse is left alone — never write over a
// config we couldn't read. Writes are atomic (temp file + rename next to the
// symlink-resolved target) so a crash can't leave a truncated config behind,
// and the rename is guarded by an optimistic recheck: if another writer (a
// live Claude Code session, `claude mcp add`) changed the file since it was
// read, the merge is redone from the fresh content instead of clobbering it.
// Claude Code honors no locking protocol, so the residual read-to-rename race
// cannot be closed entirely — but a lost update self-heals on the next
// startup re-sync.
//
// Like the skill install this is idempotent and cheap: when the existing
// entry already matches the live endpoint nothing is written. The endpoint
// (loopback URL + bearer token) comes from the ~/.fleet/mcp.port and
// mcp.token discovery files the daemon publishes when its MCP listener binds;
// until the daemon is up those don't exist, reported as ErrNotReady so a
// caller can retry instead of failing.
package admiralmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// ===========================================
// Constants
// ===========================================

// serverKey is the name the fleet MCP server is registered under in
// mcpServers. It is also the tool-name prefix Claude Code derives
// (mcp__fleet__fleet_list, ...), and must stay in sync with the server names
// used in the SKILL.md and the settings-page snippets.
const serverKey = "fleet"

// configFile is Claude Code's user-scope state file (in the user's home
// directory) — the documented location for user-scope (all-projects) MCP
// servers, under its top-level "mcpServers" key.
const configFile = ".claude.json"

// mcpServersKey is the top-level ~/.claude.json key holding user-scope MCP
// servers.
const mcpServersKey = "mcpServers"

// retryInterval paces EnsureInstalledEventually's polling for the daemon's
// endpoint files. The cold-start gap is daemon spawn + MCP bind — usually
// well under a second — so a sub-second cadence resolves the common case on
// the first or second probe without busy-spinning the slow ones.
const retryInterval = 500 * time.Millisecond

// mergeAttempts bounds the redo loop when the optimistic recheck detects a
// concurrent ~/.claude.json writer. More than one collision in a row is
// already vanishingly rare; on exhaustion the install is abandoned for this
// startup (it self-heals on the next one).
const mergeAttempts = 3

// mergeRetryDelay spaces the redo attempts so a burst of writes from a live
// Claude Code session can settle.
const mergeRetryDelay = 50 * time.Millisecond

// ===========================================
// Errors
// ===========================================

// ErrNotReady reports that the daemon's MCP endpoint discovery files
// (~/.fleet/mcp.port, mcp.token) are not readable yet — normally just "the
// daemon hasn't bound its MCP listener yet" during a cold start. Retryable,
// unlike every other error this package returns.
var ErrNotReady = errors.New("fleet MCP endpoint not published yet")

// errConcurrentChange reports that ~/.claude.json changed between read and
// rename; the merge must be redone from the fresh content.
var errConcurrentChange = errors.New("claude config changed during merge")

// ===========================================
// Public API
// ===========================================

// EnsureInstalled merges the live local MCP endpoint into ~/.claude.json as
// the user-scope server "fleet", and is a no-op when the entry is already
// current. It returns ErrNotReady while the daemon hasn't published its
// endpoint files.
func EnsureInstalled() error {
	url, token, err := localMCPEndpoint()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, configFile)

	// ~/.claude.json may be a symlink (fleet's own Claude-config mount in
	// instances, dotfile managers). Operate on the real target so the
	// temp+rename write replaces the file behind the link instead of severing
	// the link — and so the temp file lands on the target's filesystem,
	// keeping the rename atomic.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	for attempt := 1; ; attempt++ {
		err := mergeEntry(path, url, token)
		if !errors.Is(err, errConcurrentChange) || attempt == mergeAttempts {
			return err
		}
		time.Sleep(mergeRetryDelay)
	}
}

// EnsureInstalledEventually retries EnsureInstalled until it succeeds, hits a
// non-retryable error, or wait elapses, returning the last result (callers
// that just want best-effort — the TUI — ignore it). It exists for the TUI's
// cold-start race: launching the TUI auto-spawns the daemon, and the endpoint
// files only appear once the daemon's MCP listener binds.
//
// When the client is pointed at a remote daemon (FLEET_GATEWAY/FLEET_SERVER)
// it does nothing: the local discovery files — if they exist at all —
// describe a daemon the user isn't talking to. The remote MCP URL is surfaced
// on the TUI settings page for manual configuration instead.
func EnsureInstalledEventually(wait time.Duration) error {
	if os.Getenv("FLEET_GATEWAY") != "" || os.Getenv("FLEET_SERVER") != "" {
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		err := EnsureInstalled()
		if err == nil || !errors.Is(err, ErrNotReady) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(retryInterval)
	}
}

// ===========================================
// Endpoint discovery
// ===========================================

// localMCPEndpoint reads the loopback MCP URL and bearer token from the
// daemon's discovery files. Missing (or still-empty) files mean the daemon's
// MCP listener isn't up yet → ErrNotReady; present-but-garbled content is a
// real error.
func localMCPEndpoint() (url, token string, err error) {
	portRaw, err := os.ReadFile(fleetpaths.McpPortPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w (%s)", ErrNotReady, fleetpaths.McpPortPath())
	}
	if err != nil {
		// Anything but absence (permission denied, I/O error) is not "the
		// daemon isn't up yet" — fail fast instead of retrying it away.
		return "", "", err
	}
	portStr := strings.TrimSpace(string(portRaw))
	if portStr == "" {
		// The daemon's plain WriteFile truncates before it writes, so a reader
		// can catch the file empty mid-write; that's "not yet", not garbage.
		return "", "", fmt.Errorf("%w (empty %s)", ErrNotReady, fleetpaths.McpPortPath())
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", "", fmt.Errorf("invalid MCP port in %s: %q", fleetpaths.McpPortPath(), portStr)
	}

	tokenRaw, err := os.ReadFile(fleetpaths.McpTokenPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w (%s)", ErrNotReady, fleetpaths.McpTokenPath())
	}
	if err != nil {
		return "", "", err
	}
	token = strings.TrimSpace(string(tokenRaw))
	if token == "" {
		return "", "", fmt.Errorf("%w (empty %s)", ErrNotReady, fleetpaths.McpTokenPath())
	}

	return fmt.Sprintf("http://127.0.0.1:%d", port), token, nil
}

// serverEntry is the mcpServers value for the fleet server: a Streamable HTTP
// endpoint plus the bearer token the daemon requires. Literal values (not
// ${FLEET_MCP_URL} references) on purpose: env expansion only helps when the
// shell that launched Claude Code sourced ~/.fleet/mcp.env, which bash gets
// via the .bashrc wire-in but zsh/fish/GUI launches do not. Literals work
// everywhere, and staleness is a non-issue because every TUI startup re-syncs
// this entry against the live discovery files.
func serverEntry(url, token string) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  url,
		"headers": map[string]any{
			"Authorization": "Bearer " + token,
		},
	}
}

// ===========================================
// Config merge
// ===========================================

// mergeEntry performs one read-merge-write cycle against the (symlink
// resolved) config path. It returns errConcurrentChange when the file changed
// underneath the merge, and nil without writing when the entry is already
// current.
func mergeEntry(path, url, token string) error {
	root, snapshot, mode, err := readClaudeConfig(path)
	if err != nil {
		return err
	}

	servers, ok := root[mcpServersKey].(map[string]any)
	if !ok {
		if existing, present := root[mcpServersKey]; present && existing != nil {
			// A non-object mcpServers means the file is malformed; leave it
			// for the user rather than guess at a repair.
			return fmt.Errorf("%s: %q is not an object", path, mcpServersKey)
		}
		servers = map[string]any{}
	}

	want := serverEntry(url, token)
	if reflect.DeepEqual(servers[serverKey], want) {
		return nil
	}
	servers[serverKey] = want
	root[mcpServersKey] = servers

	return writeClaudeConfig(path, root, mode, snapshot)
}

// ===========================================
// Config file I/O
// ===========================================

// readClaudeConfig decodes the config into a generic tree, also returning the
// raw bytes it parsed (nil when the file doesn't exist — the snapshot for the
// pre-rename concurrency check) and the file mode to use on rewrite. The mode
// preserves the existing file's owner bits but always strips group/other: the
// rewritten document embeds the MCP bearer token, and that token — not the
// loopback port — is the MCP access boundary, so it must never be readable by
// other users (0600 for a fresh file, matching the daemon's token files).
// Numbers decode as json.Number so re-encoding reproduces them exactly
// (Claude Code stores large integer timestamps a float64 round-trip would
// mangle). A file that exists but doesn't parse as a single JSON document is
// an error: the caller must not write over a config we couldn't read.
func readClaudeConfig(path string) (root map[string]any, snapshot []byte, mode os.FileMode, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, 0o600, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}

	mode = 0o600
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm() &^ 0o077
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, data, mode, nil
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	root = map[string]any{}
	if err := dec.Decode(&root); err != nil {
		return nil, nil, 0, fmt.Errorf("parse %s: %w", path, err)
	}
	// A JSON `null` document decodes into a nil map WITHOUT an error; treat
	// it like a blank file so the merge can't panic assigning into nil.
	if root == nil {
		root = map[string]any{}
	}
	// Anything after the first document (a second object, stray text) means
	// this is not the single-JSON-object file we know how to merge into.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, 0, fmt.Errorf("parse %s: trailing data after JSON document", path)
	}
	return root, data, mode, nil
}

// writeClaudeConfig atomically replaces path with the re-encoded tree: encode
// to a buffer, write a temp file in the same directory, then rename over the
// original. Claude Code re-reads this file at session start; the rename
// guarantees it can never observe a half-written document. Immediately before
// the rename the current content is compared against the snapshot the merge
// was computed from — a mismatch means another writer got in, and renaming
// would silently revert their change, so errConcurrentChange asks the caller
// to redo the merge on the fresh content instead.
func writeClaudeConfig(path string, root map[string]any, mode os.FileMode, snapshot []byte) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), configFile+".fleet-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup on any failure below; a no-op after the rename.
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	cur, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cur = nil
	} else if err != nil {
		return err
	}
	if !bytes.Equal(cur, snapshot) {
		return errConcurrentChange
	}
	return os.Rename(tmpPath, path)
}
