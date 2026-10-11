package podinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// freeze.go freezes the user cgroup for a clone's consistent copy, and
// thaws it again. A freeze stops every user process (the start script, and
// every command the pod agent starts for a user, docker-in-docker's dockerd
// included) at once, as docker commit pauses a container; the agent, PID 1 and
// the watchdog stay in the agent cgroup and keep running, so the pod still
// answers fleetd, and an exec the kubelet starts (into PID 1's cgroup) does
// not stall.
//
// A freeze nobody thaws must not outlive its deadline, whatever dies: the
// agent that froze it (its own timer goes with it), its session, or fleetd.
// So a freeze is recorded first (FreezeMarkerFile: its deadline), and pod-init,
// which lives as long as the container, thaws a recorded freeze past its
// deadline, and one with no record at all (WatchFreeze).

// FreezeMarkerFile records the freeze in force, for the watchdog.
const FreezeMarkerFile = podseed.RunDir + "/freeze.json"

// FreezeMarker is a freeze's record.
type FreezeMarker struct {
	V int `json:"v"`
	// FrozenAt is when it began; Deadline when the watchdog thaws it.
	FrozenAt time.Time `json:"frozen_at"`
	Deadline time.Time `json:"deadline"`
	// ThawedBy names the watchdog once it thawed the freeze ("agent" or
	// "pod-init"), for the thaw that comes after to report.
	ThawedBy string    `json:"thawed_by,omitempty"`
	ThawedAt time.Time `json:"thawed_at,omitzero"`
}

// Freeze bounds and grace.
var (
	// freezeSettle bounds the wait for every user process to stop (one in
	// uninterruptible sleep on a slow disk delays it).
	freezeSettle = 10 * time.Second
	// freezeGrace is how long past a freeze's deadline pod-init waits before
	// it thaws: the agent's own timer thaws at the deadline.
	freezeGrace = 2 * time.Second
	// freezePoll is how often a freeze looks whether it settled.
	freezePoll = 10 * time.Millisecond
)

// ErrNoLayout is a pod without the cgroup layout: unprivileged, or cgroup
// v1. It cannot be frozen.
var ErrNoLayout = errNoLayout

// FreezeResult is how a freeze went.
type FreezeResult struct {
	FrozenAt time.Time
	Deadline time.Time
	Settle   time.Duration
}

// ThawResult is how a thaw went.
type ThawResult struct {
	// WasFrozen: the cgroup was frozen when the thaw came.
	WasFrozen bool
	// ByWatchdog names the watchdog that thawed it first, if one did.
	ByWatchdog string
	ThawedAt   time.Time
}

func (l *Layout) cfs() cgroupFS {
	if l.fs == nil {
		return osCgroupFS{}
	}
	return l.fs
}

// usable reports whether l is a layout a freeze can act on.
func (l *Layout) usable() error {
	if l == nil || l.User == "" || !path.IsAbs(l.User) {
		return ErrNoLayout
	}
	return nil
}

// Freeze freezes the user cgroup until a thaw, or until the watchdog thaws it
// at deadline, and waits for every process in it to stop. The freeze is
// recorded at marker before the cgroup is frozen, so the watchdog never misses
// one. A freeze that does not settle within the settle bound is thawed again
// and fails.
func (l *Layout) Freeze(marker string, now time.Time, deadline time.Time) (FreezeResult, error) {
	if err := l.usable(); err != nil {
		return FreezeResult{}, err
	}
	if err := writeMarker(marker, FreezeMarker{V: 1, FrozenAt: now, Deadline: deadline}); err != nil {
		return FreezeResult{}, fmt.Errorf("record the freeze: %w", err)
	}
	cfs := l.cfs()
	if err := cfs.WriteFile(path.Join(l.User, "cgroup.freeze"), []byte("1")); err != nil {
		_ = os.Remove(marker)
		return FreezeResult{}, fmt.Errorf("freeze %s: %w", UserCgroup, err)
	}
	start := time.Now()
	for {
		frozen, err := l.frozen()
		if err == nil && frozen {
			return FreezeResult{FrozenAt: time.Now(), Deadline: deadline, Settle: time.Since(start)}, nil
		}
		if time.Since(start) > freezeSettle {
			_, _ = l.Thaw(marker, "")
			if err == nil {
				err = fmt.Errorf("its processes did not all stop within %s", freezeSettle)
			}
			return FreezeResult{}, fmt.Errorf("freeze %s: %w", UserCgroup, err)
		}
		time.Sleep(freezePoll)
	}
}

// frozen reports whether every process of the user cgroup has stopped
// (cgroup.events "frozen 1").
func (l *Layout) frozen() (bool, error) {
	data, err := l.cfs().ReadFile(path.Join(l.User, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "frozen" {
			return f[1] == "1", nil
		}
	}
	return false, errors.New("cgroup.events has no frozen line")
}

// freezing reports whether the user cgroup is asked to be frozen
// (cgroup.freeze reads 1).
func (l *Layout) freezing() (bool, error) {
	data, err := l.cfs().ReadFile(path.Join(l.User, "cgroup.freeze"))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) == "1", nil
}

// Thaw thaws the user cgroup. by is the watchdog thawing ("" for a thaw
// asked for): a watchdog's thaw is kept in the marker for the thaw that comes
// after, which reports it and removes the marker.
func (l *Layout) Thaw(marker, by string) (ThawResult, error) {
	if err := l.usable(); err != nil {
		return ThawResult{}, err
	}
	res := ThawResult{ThawedAt: time.Now()}
	was, err := l.freezing()
	if err != nil {
		return res, fmt.Errorf("read %s's freeze: %w", UserCgroup, err)
	}
	res.WasFrozen = was
	if was {
		if err := l.cfs().WriteFile(path.Join(l.User, "cgroup.freeze"), []byte("0")); err != nil {
			return res, fmt.Errorf("thaw %s: %w", UserCgroup, err)
		}
	}
	m, merr := readMarker(marker)
	switch {
	case by != "":
		if merr == nil && m.ThawedBy == "" {
			m.ThawedBy, m.ThawedAt = by, res.ThawedAt
			_ = writeMarker(marker, m)
		}
	default:
		if merr == nil && m.ThawedBy != "" {
			res.ByWatchdog, res.ThawedAt = m.ThawedBy, m.ThawedAt
		}
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("clear the freeze's record: %w", err)
		}
	}
	return res, nil
}

// WatchFreeze is the watchdog's look at the user cgroup at now: a recorded
// freeze past its deadline (and the grace the agent's own timer has) is
// thawed, and so is a freeze with no record at all, which no thaw would ever
// come for. It returns a note when it thawed.
func (l *Layout) WatchFreeze(marker string, now time.Time) string {
	if l.usable() != nil {
		return ""
	}
	m, err := readMarker(marker)
	switch {
	case err == nil && m.ThawedBy != "":
		return ""
	case err == nil && now.Before(m.Deadline.Add(freezeGrace)):
		return ""
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		// A record that cannot be read is no deadline to wait for.
	case err != nil:
		// No record: thaw only a cgroup that is frozen.
		if frozen, ferr := l.freezing(); ferr != nil || !frozen {
			return ""
		}
	}
	res, terr := l.Thaw(marker, "pod-init")
	if terr != nil {
		return fmt.Sprintf("thawing %s past its freeze's deadline: %v", UserCgroup, terr)
	}
	if !res.WasFrozen {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("%s was frozen with no record of the freeze; thawed it", UserCgroup)
	}
	return fmt.Sprintf("%s was still frozen %s after its deadline; thawed it", UserCgroup, now.Sub(m.Deadline).Round(time.Second))
}

func readMarker(file string) (FreezeMarker, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return FreezeMarker{}, err
	}
	var m FreezeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return FreezeMarker{}, fmt.Errorf("parse %s: %w", file, err)
	}
	return m, nil
}

func writeMarker(file string, m FreezeMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
