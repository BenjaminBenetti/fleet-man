//go:build unix

package sharesync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// The policies of part B end to end: hub, trees and sides, with the real
// Credential, JSON3 and TOML3.

// mergeTable is an agent tool's root with a login, settings and a config,
// and the shared files root with ~/.claude.json, as the defaults have.
func mergeTable() syncpolicy.Table {
	return syncpolicy.NewTable(
		syncpolicy.Root{
			Path: "tool", Tool: "Tool", Default: syncpolicy.Mirror,
			Rules: []syncpolicy.Rule{
				{Pattern: ".credentials.json", Policy: syncpolicy.Credential},
				{Pattern: "auth.json", Policy: syncpolicy.Credential},
				{Pattern: "settings.json", Policy: syncpolicy.JSON3},
				{Pattern: "config.toml", Policy: syncpolicy.TOML3},
			},
		},
		syncpolicy.Root{Path: "files", Default: syncpolicy.Mirror, Rules: []syncpolicy.Rule{{Pattern: ".claude.json", Policy: syncpolicy.JSON3}}},
	)
}

// mergeRig is a rig over mergeTable with the default policies, its hub's
// normal lane slowed by normal (0: 1ms).
func mergeRig(t *testing.T, normal time.Duration, sides ...SideInfo) *rig {
	t.Helper()
	if normal == 0 {
		normal = time.Millisecond
	}
	r := &rig{t: t, base: t.TempDir(), table: mergeTable(), roots: []string{"tool", "files"}, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.hopts = HubOptions{NormalDelay: normal, BulkDelay: normal, PauseRetry: 20 * time.Millisecond, Logger: r.logs.logger()}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1, MergeSettle: -1} }
	r.openHub()
	t.Cleanup(r.close)
	for _, info := range sides {
		r.addSide(info)
	}
	return r
}

// faulty puts a side behind a faultyPeer.
func (r *rig) faulty(info SideInfo) *faultyPeer {
	r.t.Helper()
	f := &faultyPeer{}
	s := &rigSide{info: info, dir: filepath.Join(r.base, "sides", info.ID), reserved: map[string][]string{}, peer: f}
	for _, root := range r.roots {
		if err := os.MkdirAll(filepath.Join(s.dir, root), 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	r.sides[info.ID], r.order = s, append(r.order, info.ID)
	r.openSide(s)
	return f
}

// scan has a side scan and report now.
func (r *rig) scan(id string) {
	r.t.Helper()
	if err := r.sides[id].side.Rescan(context.Background()); err != nil {
		r.t.Fatalf("scan %s: %v", id, err)
	}
}

// heal brings a faulty side back and syncs.
func (r *rig) heal(id string, f *faultyPeer) {
	r.t.Helper()
	f.set(func(f *faultyPeer) { f.down, f.deaf, f.lose, f.fetchFail = false, false, false, false })
	r.hub.Kick(id)
	r.sync()
}

// wantFile checks each side's file.
func (r *rig) wantFile(root, rel string, want map[string]string) {
	r.t.Helper()
	for id, w := range want {
		if got := r.read(id, root, rel); got != w {
			r.t.Errorf("%s %s/%s = %q, want %q", id, root, rel, got, w)
		}
	}
}

var (
	host = SideInfo{ID: "host", Location: "host"}
	a1   = SideInfo{ID: "a1", Location: "k1"}
	a2   = SideInfo{ID: "a2", Location: "k1"}
	b1   = SideInfo{ID: "b1", Location: "k2"}
)

// TestLoginsStayInTheirLocation checks logging in once in a location logs
// in every copy there, at once (the priority lane, while the normal lane
// waits an hour), and never another location; the rest of the tool's
// folder syncs everywhere.
func TestLoginsStayInTheirLocation(t *testing.T) {
	r := mergeRig(t, time.Hour, host, a1, a2, b1)
	r.write("a1", "tool", ".credentials.json", claudeLogin("k1"))
	r.write("host", "tool", ".credentials.json", claudeLogin("host"))
	r.scan("a1")
	r.scan("host")
	deadline := time.Now().Add(5 * time.Second)
	for r.read("a2", "tool", ".credentials.json") != claudeLogin("k1") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.wantFile("tool", ".credentials.json", map[string]string{
		"host": claudeLogin("host"), "a1": claudeLogin("k1"), "a2": claudeLogin("k1"), "b1": "<missing>",
	})
	// A refresh in k1 reaches k1's other copy, and nowhere else.
	r.write("a2", "tool", ".credentials.json", claudeLogin("k1-2"))
	r.scan("a2")
	for r.read("a1", "tool", ".credentials.json") != claudeLogin("k1-2") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.wantFile("tool", ".credentials.json", map[string]string{
		"host": claudeLogin("host"), "a1": claudeLogin("k1-2"), "b1": "<missing>",
	})
}

// TestLogoutIsATombstone checks a logout reaches every copy of its
// location and stays: a copy that refreshed the token without having seen
// the logout does not bring it back, nor does a copy that was away
// holding the old login; a login made afterwards does come back.
func TestLogoutIsATombstone(t *testing.T) {
	r := mergeRig(t, 0, a1, a2)
	f := r.faulty(SideInfo{ID: "a3", Location: "k1"})
	r.write("a1", "tool", ".credentials.json", claudeLogin("1"))
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a2": claudeLogin("1"), "a3": claudeLogin("1")})

	// a3 hears nothing while a1 logs out; a3's Claude refreshes meanwhile.
	f.set(func(f *faultyPeer) { f.deaf = true })
	os.Remove(r.path("a1", "tool", ".credentials.json"))
	r.scan("a1")
	r.write("a3", "tool", ".credentials.json", claudeLogin("1-refreshed"))
	r.scan("a3")
	r.heal("a3", f)
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": "<missing>", "a2": "<missing>", "a3": "<missing>"})

	// A copy away with the old login loses it when it comes back.
	r.write("a2", "tool", ".credentials.json", claudeLogin("2"))
	r.sync()
	r.sides["a3"].stop()
	r.hub.Detach("a3")
	os.Remove(r.path("a2", "tool", ".credentials.json"))
	r.sync()
	r.openSide(r.sides["a3"])
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": "<missing>", "a2": "<missing>", "a3": "<missing>"})

	// A stale logout (a copy that had not seen the newer login) is refused.
	r.write("a1", "tool", ".credentials.json", claudeLogin("3"))
	r.sync()
	f.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a1", "tool", ".credentials.json", claudeLogin("4"))
	r.scan("a1")
	os.Remove(r.path("a3", "tool", ".credentials.json"))
	r.scan("a3")
	r.heal("a3", f)
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("4"), "a2": claudeLogin("4"), "a3": claudeLogin("4")})

	// A fresh login after a logout every copy saw goes everywhere.
	os.Remove(r.path("a2", "tool", ".credentials.json"))
	r.sync()
	r.write("a3", "tool", ".credentials.json", claudeLogin("5"))
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("5"), "a2": claudeLogin("5"), "a3": claudeLogin("5")})
	if c := r.conflicts(); len(c) != 0 {
		t.Errorf("conflict copies of logins: %q", c)
	}
}

// TestClearedLoginGetsTheWinnersToken checks Claude Code's answer to a
// refresh it lost (it clears its tokens) never signs the location's other
// copies out, whichever reaches the hub first: the copy that lost gets the
// winner's token.
func TestClearedLoginGetsTheWinnersToken(t *testing.T) {
	r := mergeRig(t, 0, a1)
	f := r.faulty(a2)
	r.write("a1", "tool", ".credentials.json", claudeLogin("1"))
	r.sync()

	// The winner's refresh first: a2 hears nothing of it and loses its own.
	f.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a1", "tool", ".credentials.json", claudeLogin("2"))
	r.scan("a1")
	r.write("a2", "tool", ".credentials.json", clearedClaudeLogin)
	r.scan("a2")
	r.heal("a2", f)
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("2"), "a2": claudeLogin("2")})

	// The loser's clear first (the winner's refresh is still on its way):
	// the hub keeps the old token for the loser, then the new one comes.
	r.write("a2", "tool", ".credentials.json", clearedClaudeLogin)
	r.scan("a2")
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("2"), "a2": claudeLogin("2")})
	r.write("a1", "tool", ".credentials.json", claudeLogin("3"))
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("3"), "a2": claudeLogin("3")})

	// The user logs in again on the copy whose clear was refused, before
	// the old token came back: the new login wins.
	f.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a2", "tool", ".credentials.json", clearedClaudeLogin)
	r.scan("a2")
	r.write("a2", "tool", ".credentials.json", claudeLogin("4"))
	r.scan("a2")
	r.heal("a2", f)
	r.wantFile("tool", ".credentials.json", map[string]string{"a1": claudeLogin("4"), "a2": claudeLogin("4")})
}

// TestARefusedClearOnTheOnlyCopyIsUndone checks a cleared login on the one
// copy of its location (the daemon host) gets its token back from the hub,
// which keeps each location's current login, and a logout drops the
// hub's copy.
func TestARefusedClearOnTheOnlyCopyIsUndone(t *testing.T) {
	r := mergeRig(t, 0, host, b1)
	r.write("host", "tool", ".credentials.json", claudeLogin("1"))
	r.sync()
	r.write("host", "tool", ".credentials.json", clearedClaudeLogin)
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"host": claudeLogin("1"), "b1": "<missing>"})
	os.Remove(r.path("host", "tool", ".credentials.json"))
	r.sync()
	root := r.hub.roots["tool"]
	root.mu.Lock()
	e := root.entries[key{loc: "host", path: ".credentials.json"}]
	root.mu.Unlock()
	if e == nil || e.Entry.Live() || len(e.Hist) != 0 {
		t.Errorf("after the logout the hub holds %+v, want a tombstone with no login kept", e)
	}
	if blobs, _ := os.ReadDir(root.blobs.dir); len(blobs) != 0 {
		t.Errorf("the hub still keeps %d blobs after the logout", len(blobs))
	}
	// A refresh replaces the hub's copy at once: one login kept, the
	// current one.
	r.write("host", "tool", ".credentials.json", claudeLogin("2"))
	r.sync()
	r.write("host", "tool", ".credentials.json", claudeLogin("3"))
	r.sync()
	blobs, _ := os.ReadDir(root.blobs.dir)
	if len(blobs) != 1 || blobs[0].Name() != hashOf([]byte(claudeLogin("3"))) {
		t.Errorf("the hub keeps %v, want only the current login", blobs)
	}
}

// TestATornLoginWaits checks a login file caught mid-write (Codex rewrites
// auth.json in place) never travels: the location's other copies keep the
// whole login until the write is done, then get it.
func TestATornLoginWaits(t *testing.T) {
	r := mergeRig(t, 0, a1, a2)
	whole := `{"tokens":{"access_token":"a","refresh_token":"r1"},"last_refresh":"2026-10-11T00:00:00Z"}`
	r.write("a1", "tool", "auth.json", whole)
	r.sync()
	next := strings.Replace(whole, "r1", "r2", 1)
	for _, torn := range []string{"", next[:len(next)/2]} {
		r.write("a1", "tool", "auth.json", torn)
		r.scan("a1")
		r.sync()
		r.wantFile("tool", "auth.json", map[string]string{"a2": whole})
	}
	r.write("a1", "tool", "auth.json", next)
	r.sync()
	r.wantFile("tool", "auth.json", map[string]string{"a2": next})
}

// claudeJSON is a ~/.claude.json with content.
const claudeJSON = "{\n  \"numStartups\": 12,\n  \"userID\": \"u1\",\n  \"projects\": {\n    \"/workspaces/a\": {\n      \"allowedTools\": []\n    }\n  }\n}"

// TestClaudeJSONIsNeverEmptied checks the '{}' every new instance seeds
// never replaces ~/.claude.json, whichever side holds it first, and an
// instance that writes over the refused seed before the fleet's file comes
// back has its keys merged into the fleet's file, not put in its place.
func TestClaudeJSONIsNeverEmptied(t *testing.T) {
	// The fleet's file first, then a share laid out with the seed.
	r := mergeRig(t, 0, host)
	r.write("host", "files", ".claude.json", claudeJSON)
	r.sync()
	c1 := &rigSide{info: a1, dir: filepath.Join(r.base, "sides", "a1"), reserved: map[string][]string{}}
	r.sides["a1"], r.order = c1, append(r.order, "a1")
	r.write("a1", "files", ".claude.json", "{}")
	os.MkdirAll(filepath.Join(c1.dir, "tool"), 0o755)
	r.openSide(c1)
	r.sync()
	r.wantFile("files", ".claude.json", map[string]string{"host": claudeJSON, "a1": claudeJSON})

	// An instance seeding again over it, fast forward or not.
	r.write("a1", "files", ".claude.json", "{}\n")
	r.sync()
	r.wantFile("files", ".claude.json", map[string]string{"host": claudeJSON, "a1": claudeJSON})

	// The seed refused, and Claude Code writing its first config over it
	// before the fleet's file is back: both copies' keys survive.
	f := r.faulty(SideInfo{ID: "a3", Location: "k1"})
	r.sync()
	f.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a3", "files", ".claude.json", "{}")
	r.scan("a3")
	r.write("a3", "files", ".claude.json", `{"numStartups":1,"firstStartTime":"now"}`)
	r.scan("a3")
	r.heal("a3", f)
	r.assertConverged()
	var got map[string]any
	if err := json.Unmarshal([]byte(r.read("host", "files", ".claude.json")), &got); err != nil {
		t.Fatalf("host .claude.json: %v", err)
	}
	if got["userID"] != "u1" || got["projects"] == nil || got["firstStartTime"] != "now" {
		t.Errorf("host .claude.json lost keys: %v", got)
	}

	// The seed first (a share laid out before the host attached), then the
	// fleet's file: the file wins, and the host's copy is never replaced.
	r2 := mergeRig(t, 0, a1)
	r2.write("a1", "files", ".claude.json", "{}")
	r2.sync()
	hs := &rigSide{info: host, dir: filepath.Join(r2.base, "sides", "host"), reserved: map[string][]string{}}
	r2.sides["host"], r2.order = hs, append(r2.order, "host")
	r2.write("host", "files", ".claude.json", claudeJSON)
	os.MkdirAll(filepath.Join(hs.dir, "tool"), 0o755)
	r2.openSide(hs)
	r2.sync()
	r2.wantFile("files", ".claude.json", map[string]string{"host": claudeJSON, "a1": claudeJSON})
}

// TestARefusedSeedIsNoBaseForItsMaker checks the side that made the
// fleet's ~/.claude.json, seeding '{}' over it and writing its first config
// over the refused seed before the file is back, has its keys merged into
// the fleet's file: its change was made on the seed, not on the file it
// made (no fast forward by the origin's rule either).
func TestARefusedSeedIsNoBaseForItsMaker(t *testing.T) {
	r := mergeRig(t, 0, host)
	f := r.faulty(SideInfo{ID: "a3", Location: "k1"})
	r.write("a3", "files", ".claude.json", claudeJSON)
	r.sync()
	r.wantFile("files", ".claude.json", map[string]string{"host": claudeJSON})
	f.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a3", "files", ".claude.json", "{}")
	r.scan("a3")
	r.write("a3", "files", ".claude.json", `{"numStartups":1,"firstStartTime":"now"}`)
	r.scan("a3")
	r.heal("a3", f)
	r.assertConverged()
	var got map[string]any
	if err := json.Unmarshal([]byte(r.read("host", "files", ".claude.json")), &got); err != nil {
		t.Fatalf("host .claude.json: %v", err)
	}
	if got["userID"] != "u1" || got["projects"] == nil || got["firstStartTime"] != "now" {
		t.Errorf("host .claude.json lost keys: %v", got)
	}

	// The same with the hub killed in between.
	r2 := mergeRig(t, 0, host)
	f2 := r2.faulty(SideInfo{ID: "a3", Location: "k1"})
	r2.write("a3", "files", ".claude.json", claudeJSON)
	r2.sync()
	f2.set(func(f *faultyPeer) { f.deaf = true })
	r2.write("a3", "files", ".claude.json", "{}")
	r2.scan("a3")
	r2.crashHub()
	r2.openHub()
	for _, id := range r2.order {
		r2.openSide(r2.sides[id])
	}
	r2.write("a3", "files", ".claude.json", `{"numStartups":2}`)
	r2.scan("a3")
	r2.heal("a3", f2)
	r2.assertConverged()
	got = nil
	if err := json.Unmarshal([]byte(r2.read("host", "files", ".claude.json")), &got); err != nil {
		t.Fatalf("host .claude.json: %v", err)
	}
	if got["userID"] != "u1" || got["projects"] == nil {
		t.Errorf("host .claude.json after the hub was killed lost keys: %v", got)
	}
}

// TestSettingsMergeAcrossLocations checks settings changed in two places at
// once both survive everywhere, hooks merged as sets, and the merge is the
// base of the next round.
func TestSettingsMergeAcrossLocations(t *testing.T) {
	r := mergeRig(t, 0, host, a1, b1)
	base := `{"model":"opus","hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]}]}}`
	r.write("host", "tool", "settings.json", base)
	r.sync()
	r.write("host", "tool", "settings.json", `{"model":"opus","permissions":{"allow":["Bash(ls)"]},"hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]}]}}`)
	r.write("a1", "tool", "settings.json", `{"model":"opus","hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]},{"matcher":"*","hooks":[{"type":"command","command":"mine"}]}]}}`)
	r.write("b1", "tool", "settings.json", `{"model":"sonnet","hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]}]}}`)
	r.sync()
	r.assertConverged()
	want := `{"model":"sonnet","permissions":{"allow":["Bash(ls)"]},"hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]},{"matcher":"*","hooks":[{"type":"command","command":"mine"}]}]}}`
	if got, _ := parseJSON([]byte(r.read("host", "tool", "settings.json"))); got == nil || !same(got, mustJSON(t, want)) {
		t.Errorf("merged settings.json = %s\nwant %s", r.read("host", "tool", "settings.json"), want)
	}
	// Next round, on the merge: a1 removes its hook, the host adds a rule.
	merged := r.read("a1", "tool", "settings.json")
	r.write("a1", "tool", "settings.json", strings.Replace(merged, `,{"matcher":"*","hooks":[{"type":"command","command":"mine"}]}`, "", 1))
	r.write("host", "tool", "settings.json", strings.Replace(merged, `["Bash(ls)"]`, `["Bash(ls)","Read"]`, 1))
	r.sync()
	r.assertConverged()
	want = `{"model":"sonnet","permissions":{"allow":["Bash(ls)","Read"]},"hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/fleet Stop"}]}]}}`
	if got, _ := parseJSON([]byte(r.read("b1", "tool", "settings.json"))); got == nil || !same(got, mustJSON(t, want)) {
		t.Errorf("second merge = %s\nwant %s", r.read("b1", "tool", "settings.json"), want)
	}
	if c := r.conflicts(); len(c) != 0 {
		t.Errorf("conflict copies: %q", c)
	}
}

// TestConfigTOMLMergesKeepingComments checks Codex configs changed in two
// places at once merge everywhere, comments kept.
func TestConfigTOMLMergesKeepingComments(t *testing.T) {
	r := mergeRig(t, 0, host, a1)
	r.write("host", "tool", "config.toml", codexConfig)
	r.sync()
	r.write("host", "tool", "config.toml", codexConfig+"\n[projects.\"/workspaces/h\"]\ntrust_level = \"trusted\"\n")
	r.write("a1", "tool", "config.toml", strings.Replace(codexConfig, `theme = "dark"`, `theme = "light"`, 1))
	r.sync()
	r.assertConverged()
	got := r.read("a1", "tool", "config.toml")
	for _, want := range []string{"# Codex config", "# my favourite", "# the desktop", "# Projects I trust.", "# the status line", `"/workspaces/h"`, `theme = "light"`} {
		if !strings.Contains(got, want) {
			t.Errorf("merged config.toml lacks %q:\n%s", want, got)
		}
	}
}

// TestMergesNeedNoHolder checks a settings merge does not wait for the
// side whose copy is current: the hub keeps what it merges with.
func TestMergesNeedNoHolder(t *testing.T) {
	r := mergeRig(t, 0)
	h := r.faulty(host)
	f := r.faulty(a1)
	r.write("a1", "tool", "settings.json", `{"a":1}`)
	r.sync()
	// The host hears nothing of a1's change, and a1 is gone when the host's
	// own (made on the old content) arrives.
	h.set(func(f *faultyPeer) { f.deaf = true })
	r.write("a1", "tool", "settings.json", `{"a":2}`)
	r.scan("a1")
	f.set(func(f *faultyPeer) { f.down = true })
	r.write("host", "tool", "settings.json", `{"a":1,"b":1}`)
	r.scan("host")
	h.set(func(f *faultyPeer) { f.deaf = false })
	r.hub.Kick("host")
	deadline := time.Now().Add(5 * time.Second)
	for r.read("host", "tool", "settings.json") != `{"a":2,"b":1}` && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.read("host", "tool", "settings.json"); got != `{"a":2,"b":1}` {
		t.Errorf("host settings.json = %q, want the merge made without a1", got)
	}
	r.heal("a1", f)
	r.assertConverged()
}

// TestMergeBasesSurviveARestart checks the hub finds a concurrent change's
// base after it restarts.
func TestMergeBasesSurviveARestart(t *testing.T) {
	r := mergeRig(t, 0, host, a1)
	r.write("host", "tool", "settings.json", `{"a":1,"b":1,"c":1}`)
	r.sync()
	r.restartHub()
	r.write("host", "tool", "settings.json", `{"a":2,"b":1,"c":1}`)
	r.write("a1", "tool", "settings.json", `{"a":1,"b":1}`)
	r.sync()
	r.assertConverged()
	// With the base, a1's removal of c holds; without it, c would be back.
	if got, _ := parseJSON([]byte(r.read("host", "tool", "settings.json"))); !same(got, mustJSON(t, `{"a":2,"b":1}`)) {
		t.Errorf("settings.json = %s", r.read("host", "tool", "settings.json"))
	}
}

func mustJSON(t *testing.T, doc string) *value {
	t.Helper()
	v, err := parseJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMergingFilesSettle checks a settings or login file modified a moment
// ago is not reported until it has gone unmodified for MergeSettle (a write
// in place is not caught half done), while other files go at once; a file
// whose clock runs ahead of the scan's is reported once the scan has seen
// it unchanged that long.
func TestMergingFilesSettle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool")
	os.MkdirAll(dir, 0o755)
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	tree, err := OpenTree([]TreeRoot{{Name: "tool", Dir: dir}}, TreeOptions{StateDir: t.TempDir(), Table: mergeTable(), Settle: -1, MergeSettle: time.Minute, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	scan := func(paths ...string) ([]string, []string) {
		t.Helper()
		rep, again, err := tree.Scan(context.Background(), "tool", paths)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range rep.Changes {
			got = append(got, c.Path)
		}
		slices.Sort(got)
		slices.Sort(again)
		return got, again
	}
	for _, name := range []string{"settings.json", "config.toml", ".credentials.json", "notes"} {
		os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644)
		os.Chtimes(filepath.Join(dir, name), now, now)
	}
	if got, again := scan(); !slices.Equal(got, []string{"notes"}) || !slices.Equal(again, []string{".credentials.json", "config.toml", "settings.json"}) {
		t.Errorf("fresh files: reported %q, again %q; want notes now and the merging files later", got, again)
	}
	advance(2 * time.Minute)
	// (notes again: no hub acknowledged it.)
	if got, _ := scan(); !slices.Equal(got, []string{".credentials.json", "config.toml", "notes", "settings.json"}) {
		t.Errorf("settled files: reported %q", got)
	}
	// A clock an hour ahead: seen unchanged for the settle time is enough.
	ahead := now.Add(time.Hour)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"a":1}`), 0o644)
	os.Chtimes(filepath.Join(dir, "settings.json"), ahead, ahead)
	if got, again := scan("settings.json"); len(got) != 0 || !slices.Equal(again, []string{"settings.json"}) {
		t.Errorf("a file from a clock ahead, first seen: reported %q, again %q", got, again)
	}
	advance(30 * time.Second)
	if got, _ := scan("settings.json"); len(got) != 0 {
		t.Errorf("seen unchanged for half the settle time: reported %q", got)
	}
	advance(31 * time.Second)
	if got, _ := scan("settings.json"); !slices.Equal(got, []string{"settings.json"}) {
		t.Errorf("seen unchanged for the settle time: reported %q", got)
	}
}

// TestASettlingFileGoneSettlesNoMore checks a merging file removed while
// it settles is not waited for again: a side's rescans and flushes would
// each wait out its settle and scan its folder again, for good.
func TestASettlingFileGoneSettlesNoMore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool")
	os.MkdirAll(dir, 0o755)
	tree, err := OpenTree([]TreeRoot{{Name: "tool", Dir: dir}}, TreeOptions{StateDir: t.TempDir(), Table: mergeTable(), Settle: -1, MergeSettle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	for _, name := range []string{"settings.json", "config.toml"} {
		os.WriteFile(filepath.Join(dir, name), []byte(`a = 1`), 0o644)
	}
	if _, _, err := tree.Scan(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	}
	if got := tree.settling("tool"); len(got) != 2 {
		t.Fatalf("settling %q, want both files", got)
	}
	os.Remove(filepath.Join(dir, "settings.json"))
	// A scan of another path leaves it be; one that covers it does not.
	if _, _, err := tree.Scan(context.Background(), "tool", []string{"config.toml"}); err != nil {
		t.Fatal(err)
	}
	if got := tree.settling("tool"); len(got) != 2 {
		t.Errorf("settling %q after a scan elsewhere, want both files", got)
	}
	if _, _, err := tree.Scan(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	}
	if got := tree.settling("tool"); !slices.Equal(got, []string{"config.toml"}) {
		t.Errorf("settling %q, want only the file still there", got)
	}
}

// TestFlushWaitsOutTheSettle checks a flush right after a login or settings
// file was written delivers it, the settle waited out rather than the file
// left behind.
func TestFlushWaitsOutTheSettle(t *testing.T) {
	r := mergeRig(t, 0)
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1, MergeSettle: 50 * time.Millisecond} }
	r.addSide(a1)
	r.addSide(a2)
	r.write("a1", "tool", ".credentials.json", claudeLogin("1"))
	r.write("a1", "tool", "settings.json", `{"a":1}`)
	r.sync()
	r.wantFile("tool", ".credentials.json", map[string]string{"a2": claudeLogin("1")})
	r.wantFile("tool", "settings.json", map[string]string{"a2": `{"a":1}`})
}
