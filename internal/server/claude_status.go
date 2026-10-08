package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// claude_status.go feeds the fleet status mod (agentstrategy.StatusModName),
// the Claude Code mod in every instance that draws a band above the prompt.
// While the global "Fleet status mod" setting is on, the daemon keeps a status
// file (agentstrategy.StatusModFileName) in the control directory of every
// running instance whose container mounts it: that instance's fleet/instance
// name, the agents working and idle across every fleet, and the agents of
// other instances that just stopped — went from working to idle, the TUI's ⏸.
// With the setting off the files are removed, and the mod draws nothing.
//
// Agent activity is only polled while a TUI subscribes to runtime (the
// pollers' gate), so the files are only refreshed then; the mod stops trusting
// the counts once the file stops changing.
//
// The control directory is writable from inside the instance, so the file is
// written by temp+rename (atomicfile) and removed by unlink: a symlink planted
// at its name is replaced or removed, never followed.

const (
	// claudeStatusInterval is how often the files are brought up to date.
	claudeStatusInterval = 2 * time.Second
	// claudeStatusHeartbeat is how often an unchanged file is rewritten anyway,
	// so the mod can tell live counts from a file nobody refreshes.
	claudeStatusHeartbeat = 10 * time.Second
	// claudeStatusStopWindow is how long a stop stays in the files.
	claudeStatusStopWindow = 30 * time.Second
	// claudeStatusWarmup is how long after the activity polling (re)starts
	// changes of activity are not taken for stops: until the first polls land,
	// the runtime holds whatever activity was last seen, maybe long ago.
	claudeStatusWarmup = 2 * statsActivityInterval
)

// claudeStatus keeps the status files. Its methods run on its own goroutine.
type claudeStatus struct {
	h *hub

	// prev is each running instance's agent activity at the last update, by
	// runtime key; nil while activity is not being polled.
	prev map[string]fleetgrpc.AgentActivity
	// trackFrom is when changes of activity start counting as stops.
	trackFrom time.Time
	// stops are the recent stops, oldest first.
	stops []claudeStatusStop
	// written is what was last written to each control directory.
	written map[string]claudeStatusWritten
	// cleared is set once the files are removed for the setting being off.
	cleared bool
}

// claudeStatusStop is one stop, with the instance it happened in.
type claudeStatusStop struct {
	fleet, instance string
	stop            agentstrategy.StatusModStop
}

// claudeStatusWritten is a file as last written: its content less UpdatedAt.
type claudeStatusWritten struct {
	body []byte
	at   time.Time
}

func newClaudeStatus(h *hub) *claudeStatus {
	return &claudeStatus{h: h, written: make(map[string]claudeStatusWritten)}
}

// run keeps the files until ctx is cancelled.
func (c *claudeStatus) run(ctx context.Context) {
	ticker := time.NewTicker(claudeStatusInterval)
	defer ticker.Stop()
	for {
		c.tick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick brings the files in line with the setting and the agents' activity.
func (c *claudeStatus) tick(now time.Time) {
	cfg, err := state.LoadConfig()
	if err != nil {
		return
	}
	st, err := state.Load()
	if err != nil {
		return
	}
	if !cfg.ClaudeCodeSettings.StatusModEnabled() {
		c.removeAll(st)
		return
	}
	c.cleared = false
	if !c.h.runtimeWanted.Load() {
		c.pause()
		return
	}
	acts, ok := c.activities()
	if !ok {
		return
	}
	c.update(st, acts, now)
}

// activities reads every instance's agent activity off the hub, by runtime key.
func (c *claudeStatus) activities() (map[string]fleetgrpc.AgentActivity, bool) {
	ch := make(chan map[string]fleetgrpc.AgentActivity, 1)
	if !c.h.post(func(h *hub) {
		acts := make(map[string]fleetgrpc.AgentActivity, len(h.runtime))
		for key, r := range h.runtime {
			acts[key] = r.GetAgentActivity()
		}
		ch <- acts
	}) {
		return nil, false
	}
	select {
	case acts := <-ch:
		return acts, true
	case <-c.h.done:
		return nil, false
	}
}

// pause forgets the activity while it is not polled: a change seen across the
// gap is no "just stopped".
func (c *claudeStatus) pause() {
	c.prev = nil
	c.trackFrom = time.Time{}
}

// update counts the agents of every running instance, records the stops since
// the last update, and writes each mounted instance its file.
func (c *claudeStatus) update(st *state.State, acts map[string]fleetgrpc.AgentActivity, now time.Time) {
	if c.prev == nil {
		c.trackFrom = now.Add(claudeStatusWarmup)
	}
	tracking := !now.Before(c.trackFrom)

	working, idle := 0, 0
	prev := make(map[string]fleetgrpc.AgentActivity)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status != fleet.StatusRunning {
				continue
			}
			key := runtimeKey(fleetName, inst.Name)
			act := acts[key]
			switch act {
			case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING:
				working++
			case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING:
				idle++
			}
			if tracking && c.prev[key] == fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING &&
				act == fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING {
				c.stops = append(c.stops, claudeStatusStop{
					fleet:    fleetName,
					instance: inst.Name,
					stop:     agentstrategy.StatusModStop{Fleet: fleetName, Instance: inst.GetDisplayName(), At: now.UnixMilli()},
				})
			}
			prev[key] = act
		}
	}
	c.prev = prev

	cutoff := now.Add(-claudeStatusStopWindow).UnixMilli()
	for len(c.stops) > 0 && c.stops[0].stop.At < cutoff {
		c.stops = c.stops[1:]
	}

	seen := make(map[string]bool)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status != fleet.StatusRunning {
				continue
			}
			dir := state.ControlDir(fleetName, inst.Name)
			if !controlDirMounted(dir) {
				continue
			}
			seen[dir] = true
			file := agentstrategy.StatusModFile{
				Fleet:    fleetName,
				Instance: inst.GetDisplayName(),
				Working:  working,
				Idle:     idle,
				Stops:    c.stopsElsewhere(fleetName, inst.Name),
			}
			c.write(dir, file, now)
		}
	}
	for dir := range c.written {
		if !seen[dir] {
			delete(c.written, dir)
		}
	}
}

// stopsElsewhere is the recent stops of every instance but this one: its own
// agent stopping is right there on its screen.
func (c *claudeStatus) stopsElsewhere(fleetName, instanceName string) []agentstrategy.StatusModStop {
	stops := make([]agentstrategy.StatusModStop, 0, len(c.stops))
	for _, s := range c.stops {
		if s.fleet != fleetName || s.instance != instanceName {
			stops = append(stops, s.stop)
		}
	}
	return stops
}

// write writes dir's status file when its content changed, or when it is due
// a heartbeat. A failed write is tried again on the next update.
func (c *claudeStatus) write(dir string, file agentstrategy.StatusModFile, now time.Time) {
	body, err := json.Marshal(file)
	if err != nil {
		return
	}
	last, ok := c.written[dir]
	if ok && bytes.Equal(last.body, body) && now.Sub(last.at) < claudeStatusHeartbeat {
		return
	}
	file.UpdatedAt = now.UnixMilli()
	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	if err := atomicfile.Write(filepath.Join(dir, agentstrategy.StatusModFileName), data, 0o644); err != nil {
		return
	}
	c.written[dir] = claudeStatusWritten{body: body, at: now}
}

// removeAll removes every instance's status file, once per time the setting is
// turned off (and once at startup with it off, for files a previous daemon
// left behind).
func (c *claudeStatus) removeAll(st *state.State) {
	c.pause()
	c.stops = nil
	if c.cleared {
		return
	}
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			_ = os.Remove(filepath.Join(state.ControlDir(fleetName, inst.Name), agentstrategy.StatusModFileName))
		}
	}
	c.written = make(map[string]claudeStatusWritten)
	c.cleared = true
}
