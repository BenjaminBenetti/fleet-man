package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// claude_status_test.go: the fleet status mod's files.

const (
	working = fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING
	waiting = fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING
	absent  = fleetgrpc.AgentActivity_AGENT_ACTIVITY_NOT_RUNNING
)

// statusFleets is two fleets: web (alpha displayed as "Alpha", beta — both
// mounted) and api (gamma mounted, delta not mounted, old stopped).
func statusFleets(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	provisionControlDir(t, "web", "alpha", true)
	provisionControlDir(t, "web", "beta", true)
	provisionControlDir(t, "api", "gamma", true)
	provisionControlDir(t, "api", "delta", false)
	provisionControlDir(t, "api", "old", true)
	running := func(name, display string) *fleet.Instance {
		return &fleet.Instance{Name: name, DisplayName: display, Status: fleet.StatusRunning}
	}
	return &state.State{Fleets: map[string]*fleet.Fleet{
		"web": {Name: "web", Instances: []*fleet.Instance{running("alpha", "Alpha"), running("beta", "")}},
		"api": {Name: "api", Instances: []*fleet.Instance{
			running("gamma", ""), running("delta", ""),
			{Name: "old", Status: fleet.StatusStopped},
		}},
	}}
}

func readStatusFile(t *testing.T, fleetName, instanceName string) (agentstrategy.StatusModFile, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state.ControlDir(fleetName, instanceName), agentstrategy.StatusModFileName))
	if os.IsNotExist(err) {
		return agentstrategy.StatusModFile{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var f agentstrategy.StatusModFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s/%s: %v (%s)", fleetName, instanceName, err, data)
	}
	return f, true
}

// stalePass is when an activity pass from long before the tests' polling
// started began: the runtime may still hold its activity.
var stalePass = time.UnixMilli(1)

// TestClaudeStatusCountsEveryFleet: every mounted running instance gets a file
// naming itself, with the agents of every fleet's running instances counted.
func TestClaudeStatusCountsEveryFleet(t *testing.T) {
	st := statusFleets(t)
	c := newClaudeStatus(newHub())
	acts := map[string]fleetgrpc.AgentActivity{
		"web/alpha": working, "web/beta": waiting,
		"api/gamma": working, "api/delta": waiting,
		"api/old": working, // stopped instance: a stale runtime entry, not counted
	}
	// Until a pass that started since polling (re)started lands, the runtime
	// may hold activity from long before: not live, no counts.
	t0 := time.UnixMilli(1_000_000)
	c.update(st, acts, stalePass, t0)
	if a, _ := readStatusFile(t, "web", "alpha"); a.Live || a.Working != 0 || a.Instance != "Alpha" {
		t.Fatalf("web/alpha before a fresh pass = %+v, want its name only, not live", a)
	}
	now := t0.Add(claudeStatusInterval)
	c.update(st, acts, now, now)

	alpha, ok := readStatusFile(t, "web", "alpha")
	if !ok {
		t.Fatal("web/alpha has no status file")
	}
	want := agentstrategy.StatusModFile{
		UpdatedAt: now.UnixMilli(), Live: true, Fleet: "web", Instance: "Alpha",
		Working: 2, Idle: 2, Stops: []agentstrategy.StatusModStop{},
	}
	if alpha.UpdatedAt != want.UpdatedAt || !alpha.Live || alpha.Fleet != want.Fleet || alpha.Instance != want.Instance ||
		alpha.Working != want.Working || alpha.Idle != want.Idle || len(alpha.Stops) != 0 {
		t.Fatalf("web/alpha = %+v, want %+v", alpha, want)
	}
	if gamma, _ := readStatusFile(t, "api", "gamma"); gamma.Fleet != "api" || gamma.Instance != "gamma" || gamma.Working != 2 {
		t.Fatalf("api/gamma = %+v", gamma)
	}
	if _, ok := readStatusFile(t, "api", "delta"); ok {
		t.Error("an instance that does not mount its control directory got a file")
	}
	if _, ok := readStatusFile(t, "api", "old"); ok {
		t.Error("a stopped instance got a file")
	}
}

// TestClaudeStatusReportsStopsElsewhere: an agent going from working to idle
// between two live updates is a stop in every other instance's file (not its
// own), for the stop window.
func TestClaudeStatusReportsStopsElsewhere(t *testing.T) {
	st := statusFleets(t)
	c := newClaudeStatus(newHub())
	t0 := time.UnixMilli(1_000_000)
	acts := map[string]fleetgrpc.AgentActivity{"web/alpha": working, "web/beta": working, "api/gamma": waiting}
	c.update(st, acts, t0, t0)

	tStop := t0.Add(claudeStatusInterval)
	acts["web/alpha"] = waiting
	c.update(st, acts, tStop, tStop)
	want := agentstrategy.StatusModStop{Fleet: "web", Instance: "Alpha", At: tStop.UnixMilli()}
	for _, other := range [][2]string{{"web", "beta"}, {"api", "gamma"}} {
		f, _ := readStatusFile(t, other[0], other[1])
		if len(f.Stops) != 1 || f.Stops[0] != want {
			t.Errorf("%s/%s stops = %+v, want [%+v]", other[0], other[1], f.Stops, want)
		}
	}
	if own, _ := readStatusFile(t, "web", "alpha"); len(own.Stops) != 0 {
		t.Errorf("an instance was told of its own stop: %+v", own.Stops)
	}

	// Gone once the window has passed.
	tGone := tStop.Add(claudeStatusStopWindow + time.Millisecond)
	c.update(st, acts, tGone, tGone)
	if g, _ := readStatusFile(t, "api", "gamma"); len(g.Stops) != 0 {
		t.Fatalf("a stop outlived its window: %+v", g.Stops)
	}

	// An agent exiting is no stop.
	tLater := tStop.Add(time.Minute)
	acts["web/alpha"] = working
	c.update(st, acts, tLater, tLater)
	acts["web/alpha"] = absent
	c.update(st, acts, tLater.Add(claudeStatusInterval), tLater.Add(claudeStatusInterval))
	if g, _ := readStatusFile(t, "api", "gamma"); len(g.Stops) != 0 {
		t.Fatalf("an agent exiting was reported as a stop: %+v", g.Stops)
	}
}

// TestClaudeStatusNoStopFromBeforePollingResumed: when polling resumes the
// runtime still holds the activity from before the gap, however long it lasts
// (the first activity pass can take a while). An agent that went idle in the
// gap is no "just stopped" when the fresh pass lands — and the stale activity
// is never shown as live counts meanwhile.
func TestClaudeStatusNoStopFromBeforePollingResumed(t *testing.T) {
	st := statusFleets(t)
	c := newClaudeStatus(newHub())
	stale := map[string]fleetgrpc.AgentActivity{"web/alpha": working}
	t0 := time.UnixMilli(1_000_000)
	// A slow first pass: many updates before it lands, all from stale runtime.
	for i := 0; i < 10; i++ {
		now := t0.Add(time.Duration(i) * claudeStatusInterval)
		c.update(st, stale, stalePass, now)
		if g, _ := readStatusFile(t, "api", "gamma"); g.Live || g.Working != 0 {
			t.Fatalf("api/gamma before the first fresh pass = %+v, want not live, no counts", g)
		}
	}
	tPass := t0.Add(10 * claudeStatusInterval)
	c.update(st, map[string]fleetgrpc.AgentActivity{"web/alpha": waiting}, tPass, tPass)
	if g, _ := readStatusFile(t, "api", "gamma"); !g.Live || g.Idle != 1 || len(g.Stops) != 0 {
		t.Fatalf("api/gamma once the fresh pass landed = %+v, want live, alpha idle, no stop", g)
	}
}

// TestClaudeStatusGoesNotLiveWithoutPolling: when the last TUI leaves, every
// file is rewritten not live, its counts and recent stops dropped, so a
// session started then shows only the name — not counts or a stop from
// before, as if they were current.
func TestClaudeStatusGoesNotLiveWithoutPolling(t *testing.T) {
	st := statusFleets(t)
	c := newClaudeStatus(newHub())
	t0 := time.UnixMilli(1_000_000)
	acts := map[string]fleetgrpc.AgentActivity{"web/alpha": working, "web/beta": working}
	c.update(st, acts, t0, t0)
	tStop := t0.Add(claudeStatusInterval)
	acts["web/alpha"] = waiting
	c.update(st, acts, tStop, tStop)
	if g, _ := readStatusFile(t, "api", "gamma"); !g.Live || len(g.Stops) != 1 {
		t.Fatalf("api/gamma before the TUI left = %+v, want live with one stop", g)
	}

	c.pause()
	c.update(st, nil, time.Time{}, tStop.Add(claudeStatusInterval))
	g, ok := readStatusFile(t, "api", "gamma")
	if !ok || g.Live || g.Working != 0 || g.Idle != 0 || len(g.Stops) != 0 || g.Instance != "gamma" {
		t.Fatalf("api/gamma with no TUI = %+v, want its name only, not live", g)
	}

	// Polling again, inside the stop window: the stop from before the gap
	// does not come back, neither before a fresh pass lands nor after.
	tBack := tStop.Add(3 * claudeStatusInterval)
	c.update(st, acts, tStop, tBack) // the runtime's last pass is from before the gap
	if g, _ := readStatusFile(t, "api", "gamma"); g.Live || len(g.Stops) != 0 {
		t.Fatalf("api/gamma before a fresh pass = %+v, want not live, no stops", g)
	}
	tFresh := tBack.Add(claudeStatusInterval)
	c.update(st, acts, tFresh, tFresh)
	if g, _ := readStatusFile(t, "api", "gamma"); !g.Live || len(g.Stops) != 0 {
		t.Fatalf("api/gamma after the TUI came back = %+v, want live with no stops", g)
	}
}

// TestClaudeStatusRewritesOnChangeOrHeartbeat: an unchanged file is left alone
// until its heartbeat; a change is written at once.
func TestClaudeStatusRewritesOnChangeOrHeartbeat(t *testing.T) {
	st := statusFleets(t)
	c := newClaudeStatus(newHub())
	acts := map[string]fleetgrpc.AgentActivity{"web/alpha": working}
	updatedAt := func() int64 {
		f, _ := readStatusFile(t, "web", "alpha")
		return f.UpdatedAt
	}
	at := func(t time.Time) { c.update(st, acts, t, t) }

	t0 := time.UnixMilli(1_000_000)
	at(t0)
	at(t0.Add(claudeStatusInterval))
	if got := updatedAt(); got != t0.UnixMilli() {
		t.Fatalf("an unchanged file was rewritten before its heartbeat (updated_at %d)", got)
	}
	tChange := t0.Add(2 * claudeStatusInterval)
	acts["web/beta"] = working
	at(tChange)
	if got := updatedAt(); got != tChange.UnixMilli() {
		t.Fatalf("a change was not written at once (updated_at %d)", got)
	}
	tBeat := tChange.Add(claudeStatusHeartbeat)
	at(tBeat)
	if got := updatedAt(); got != tBeat.UnixMilli() {
		t.Fatalf("no heartbeat (updated_at %d)", got)
	}

	// Not live there is nothing to keep fresh: no heartbeat.
	tOff := tBeat.Add(claudeStatusInterval)
	c.pause()
	c.update(st, nil, time.Time{}, tOff)
	c.update(st, nil, time.Time{}, tOff.Add(2*claudeStatusHeartbeat))
	if got := updatedAt(); got != tOff.UnixMilli() {
		t.Fatalf("a not-live file got a heartbeat (updated_at %d)", got)
	}

	// But a file deleted from inside the instance comes back on the next
	// update, heartbeat or not.
	if err := os.Remove(filepath.Join(state.ControlDir("web", "alpha"), agentstrategy.StatusModFileName)); err != nil {
		t.Fatal(err)
	}
	tBack := tOff.Add(3 * claudeStatusHeartbeat)
	c.update(st, nil, time.Time{}, tBack)
	if f, ok := readStatusFile(t, "web", "alpha"); !ok || f.Instance != "Alpha" {
		t.Fatalf("a deleted not-live file was not restored: %+v (present %v)", f, ok)
	}
}

// TestClaudeStatusNeverFollowsAPlantedSymlink: the instance can write to its
// control directory; a link it plants at the file's name is replaced on write
// and removed on clear, and what it points at is untouched.
func TestClaudeStatusNeverFollowsAPlantedSymlink(t *testing.T) {
	st := statusFleets(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(state.ControlDir("web", "alpha"), agentstrategy.StatusModFileName)
	plant := func() {
		_ = os.Remove(link)
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
	}

	plant()
	c := newClaudeStatus(newHub())
	c.update(st, map[string]fleetgrpc.AgentActivity{}, time.UnixMilli(1_000_000), time.UnixMilli(1_000_000))
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the planted link was not replaced by a file (%v)", err)
	}

	plant()
	c.removeAll(st)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the planted link was not removed (%v)", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("the link's target was changed: %q", got)
	}
}

// TestClaudeStatusFollowsTheSetting: with the setting off every file goes (a
// previous daemon's too); with it on, every file names its instance, and is
// live only while a TUI has the runtime polled.
func TestClaudeStatusFollowsTheSetting(t *testing.T) {
	st := statusFleets(t)
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	c := newClaudeStatus(h)
	done := make(chan struct{})
	go func() { defer close(done); h.run(t.Context()) }()
	t.Cleanup(func() { <-done })

	setStatusMod := func(on bool) {
		cfg := state.DefaultConfig()
		cfg.ClaudeCodeSettings.StatusMod = &on
		if err := state.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	hasFile := func() bool { _, ok := readStatusFile(t, "web", "alpha"); return ok }

	// No subscriber: nothing is polled, so the file only names the instance.
	setStatusMod(true)
	c.tick(time.UnixMilli(1_000_000))
	if f, ok := readStatusFile(t, "web", "alpha"); !ok || f.Live || f.Instance != "Alpha" {
		t.Fatalf("with no runtime polled: %+v (present %v), want the name, not live", f, ok)
	}

	// A TUI subscribes: live once an activity pass that started since then
	// has landed on the hub, not before.
	h.runtimeWanted.Store(true)
	c.tick(time.UnixMilli(1_002_000))
	if f, _ := readStatusFile(t, "web", "alpha"); f.Live {
		t.Fatalf("live before an activity pass landed: %+v", f)
	}
	h.post(func(h *hub) { h.activityPassAt = time.UnixMilli(1_003_000) })
	c.tick(time.UnixMilli(1_004_000))
	if f, _ := readStatusFile(t, "web", "alpha"); !f.Live {
		t.Fatalf("not live once a fresh activity pass landed: %+v", f)
	}

	setStatusMod(false)
	c.tick(time.UnixMilli(1_006_000))
	if hasFile() {
		t.Fatal("the file outlived the setting")
	}

	// A fresh daemon with the setting off removes what an older one left.
	if err := os.WriteFile(filepath.Join(state.ControlDir("web", "beta"), agentstrategy.StatusModFileName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	newClaudeStatus(h).tick(time.UnixMilli(1_008_000))
	if _, ok := readStatusFile(t, "web", "beta"); ok {
		t.Fatal("a leftover file survived a daemon started with the setting off")
	}
}
