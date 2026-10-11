//go:build linux

package podagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// freeze_linux.go serves the freeze, thaw and sync RPCs a clone's consistent
// copy uses (podwire.MethodFreeze). The freeze is the pod's, not the
// session's: one agent may freeze and a later one (after a takeover) thaw, so
// its timer lives here, beside the record pod-init's watchdog reads.

// podFreeze is the pod's freeze in force, if this agent made it.
var podFreeze struct {
	mu    sync.Mutex
	timer *time.Timer
}

// layout is the pod's cgroup layout, or podwire.CodeUnsupported for a pod
// without one (unprivileged: a read-only cgroupfs).
func (a *agent) layout() (*podinit.Layout, error) {
	if a.opts.LayoutFile == "" {
		return nil, &podwire.RemoteError{Code: podwire.CodeUnsupported, Message: "this agent knows no cgroup layout"}
	}
	l, err := podinit.ReadLayout(a.opts.LayoutFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &podwire.RemoteError{Code: podwire.CodeUnsupported, Message: "the pod has no cgroup layout to freeze (an unprivileged pod)"}
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// userCgroup is the cgroup an agent with o starts commands in: the layout's
// user cgroup, "" without one. Every command is a user's (or runs as one
// would, as the docker exec it stands for): a freeze stops them all, and an
// OOM in the user cgroup picks among them, never the agent.
func userCgroup(o Options) string {
	if o.LayoutFile == "" {
		return ""
	}
	l, err := podinit.ReadLayout(o.LayoutFile)
	if err != nil {
		return ""
	}
	return l.User
}

func (a *agent) rpcFreeze(_ context.Context, raw json.RawMessage) (any, error) {
	var args podwire.FreezeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &podwire.RemoteError{Code: podwire.CodeInvalid, Message: err.Error()}
		}
	}
	l, err := a.layout()
	if err != nil {
		return nil, err
	}
	podFreeze.mu.Lock()
	defer podFreeze.mu.Unlock()
	if podFreeze.timer != nil {
		podFreeze.timer.Stop()
		podFreeze.timer = nil
	}
	now := time.Now()
	res, err := l.Freeze(a.opts.FreezeMarker, now, now.Add(args.Watchdog()))
	if err != nil {
		return nil, err
	}
	marker, log := a.opts.FreezeMarker, a.log
	podFreeze.timer = time.AfterFunc(time.Until(res.Deadline), func() {
		if th, err := l.Thaw(marker, "agent"); err != nil {
			log.Error("the freeze's watchdog could not thaw the user cgroup", "err", err.Error())
		} else if th.WasFrozen {
			log.Warn("the freeze outlasted its deadline; the watchdog thawed the user cgroup", "deadline", res.Deadline.Format(time.RFC3339Nano))
		}
	})
	info := podwire.FreezeInfo{FrozenAt: res.FrozenAt, Deadline: res.Deadline, SettleMS: res.Settle.Milliseconds()}
	took, err := syncDisk(a.opts.SyncPath)
	info.SyncMS = took.Milliseconds()
	if err != nil {
		a.log.Warn("flushing the instance disk after the freeze", "err", err.Error())
	}
	return info, nil
}

func (a *agent) rpcThaw(context.Context, json.RawMessage) (any, error) {
	l, err := a.layout()
	if err != nil {
		return nil, err
	}
	podFreeze.mu.Lock()
	defer podFreeze.mu.Unlock()
	if podFreeze.timer != nil {
		podFreeze.timer.Stop()
		podFreeze.timer = nil
	}
	res, err := l.Thaw(a.opts.FreezeMarker, "")
	if err != nil {
		return nil, err
	}
	return podwire.ThawInfo{WasFrozen: res.WasFrozen, ByWatchdog: res.ByWatchdog != "", ThawedAt: res.ThawedAt}, nil
}

func (a *agent) rpcSync(context.Context, json.RawMessage) (any, error) {
	took, err := syncDisk(a.opts.SyncPath)
	if err != nil {
		return nil, err
	}
	return podwire.SyncInfo{SyncMS: took.Milliseconds()}, nil
}

// syncDisk flushes the filesystem holding p (the instance disk, where the
// pod's persisted root lives), or every filesystem when p is "".
func syncDisk(p string) (time.Duration, error) {
	start := time.Now()
	if p == "" {
		unix.Sync()
		return time.Since(start), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()
	if err := unix.Syncfs(int(f.Fd())); err != nil {
		return 0, fmt.Errorf("syncfs %s: %w", p, err)
	}
	return time.Since(start), nil
}
