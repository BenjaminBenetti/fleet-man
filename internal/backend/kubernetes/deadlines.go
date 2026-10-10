package kubernetes

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

// deadlines.go is how long fleet waits on the cluster before it gives a
// create, start or job up, and what it calls each give-up. Every wait has a
// default and an environment override (FLEET_KUBE_*_TIMEOUT, a Go duration
// such as "90s" or "20m", or whole seconds), read once per process.

// The deadline overrides.
const (
	EnvPVCBindTimeout       = "FLEET_KUBE_PVC_BIND_TIMEOUT"
	EnvUnschedulableTimeout = "FLEET_KUBE_UNSCHEDULABLE_TIMEOUT"
	EnvImagePullTimeout     = "FLEET_KUBE_IMAGE_PULL_TIMEOUT"
	// EnvSeedTimeout replaces the seed Job's size-based deadline
	// (jobboot.SeedDeadline) with a fixed one.
	EnvSeedTimeout        = "FLEET_KUBE_SEED_TIMEOUT"
	EnvPodReadyTimeout    = "FLEET_KUBE_POD_READY_TIMEOUT"
	EnvAgentTimeout       = "FLEET_KUBE_AGENT_TIMEOUT"
	EnvSharedStateTimeout = "FLEET_KUBE_SHARED_STATE_TIMEOUT"
)

// The deadlines' defaults.
const (
	// DefaultPVCBindTimeout: a disk that has not bound by then never will
	// without someone fixing the storage class.
	DefaultPVCBindTimeout = 5 * time.Minute
	// DefaultUnschedulableTimeout: a pod the scheduler has refused for that
	// long waits on capacity nobody is adding.
	DefaultUnschedulableTimeout = 3 * time.Minute
	// DefaultImagePullTimeout covers a large first pull over a slow link.
	DefaultImagePullTimeout = 15 * time.Minute
	// DefaultPodReadyTimeout is the pod's own start: time spent on the
	// disk, the scheduler or the pull does not count.
	DefaultPodReadyTimeout = 10 * time.Minute
	// DefaultAgentTimeout bounds a pod agent's whole bootstrap
	// (verification, probe, stage, handshake).
	DefaultAgentTimeout = 2 * time.Minute
	// DefaultSharedStateTimeout is how long a create or start waits for the
	// shared state to sync into the pod before it goes on with a warning.
	DefaultSharedStateTimeout = 60 * time.Second
)

// The error codes of a wait that ran out.
const (
	CodePVCPending    = "kubernetes:pvc_pending"
	CodeUnschedulable = "kubernetes:unschedulable"
	CodeImagePull     = kubeimage.CodeImagePull
	CodeTimeout       = "kubernetes:timeout"
)

// Deadlines are the waits' bounds.
type Deadlines struct {
	PVCBind       time.Duration
	Unschedulable time.Duration
	ImagePull     time.Duration
	// Seed, when set, is every seed Job's deadline; zero sizes it to the
	// image (SeedDeadline).
	Seed        time.Duration
	PodReady    time.Duration
	Agent       time.Duration
	SharedState time.Duration
}

var (
	deadlinesOnce sync.Once
	deadlines     Deadlines
)

// DefaultDeadlines are the defaults with this process's overrides, read on
// first use. A malformed override keeps the default and says so in
// fleet.log.
func DefaultDeadlines() Deadlines {
	deadlinesOnce.Do(func() {
		var bad []string
		deadlines, bad = LoadDeadlines(os.Getenv)
		for _, b := range bad {
			flog.Warn("kube: ignoring a malformed deadline override", "override", b)
		}
	})
	return deadlines
}

// LoadDeadlines reads the overrides through getenv, returning the
// deadlines and the overrides it could not use ("NAME=value").
func LoadDeadlines(getenv func(string) string) (Deadlines, []string) {
	d := Deadlines{
		PVCBind:       DefaultPVCBindTimeout,
		Unschedulable: DefaultUnschedulableTimeout,
		ImagePull:     DefaultImagePullTimeout,
		PodReady:      DefaultPodReadyTimeout,
		Agent:         DefaultAgentTimeout,
		SharedState:   DefaultSharedStateTimeout,
	}
	var bad []string
	for _, o := range []struct {
		env string
		to  *time.Duration
	}{
		{EnvPVCBindTimeout, &d.PVCBind},
		{EnvUnschedulableTimeout, &d.Unschedulable},
		{EnvImagePullTimeout, &d.ImagePull},
		{EnvSeedTimeout, &d.Seed},
		{EnvPodReadyTimeout, &d.PodReady},
		{EnvAgentTimeout, &d.Agent},
		{EnvSharedStateTimeout, &d.SharedState},
	} {
		v := strings.TrimSpace(getenv(o.env))
		if v == "" {
			continue
		}
		if t, ok := parseTimeout(v); ok {
			*o.to = t
		} else {
			bad = append(bad, o.env+"="+v)
		}
	}
	return d, bad
}

// parseTimeout reads a Go duration or whole seconds; it must be positive.
func parseTimeout(v string) (time.Duration, bool) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 || n > int64(365*24*time.Hour/time.Second) {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// SeedDeadline is a seed Job's activeDeadlineSeconds for an image of
// imageBytes: the override when set, else jobboot's size-based one.
func (d Deadlines) SeedDeadline(imageBytes int64) time.Duration {
	if d.Seed > 0 {
		return d.Seed
	}
	return jobboot.SeedDeadline(imageBytes)
}

// DeadlineError is a wait on the cluster that ran out: Kind is its code
// (CodePVCPending, CodeUnschedulable, CodeImagePull, CodeTimeout), Wait what
// fleet waited for ("the instance's disk to bind"), After how long, and
// Detail what the cluster last said about it.
type DeadlineError struct {
	Kind   string
	Wait   string
	After  time.Duration
	Detail string
	// Env is the override that sets this deadline.
	Env string
	// Err is what the wait itself ended with, if anything did.
	Err error
}

func (e *DeadlineError) Error() string {
	msg := fmt.Sprintf("%s: gave up waiting for %s after %s", e.Kind, e.Wait, e.After.Round(time.Second))
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Env != "" {
		msg += " (" + e.Env + " sets this deadline)"
	}
	return msg
}

// Code is the error's kubernetes: code.
func (e *DeadlineError) Code() string { return e.Kind }

func (e *DeadlineError) Unwrap() error { return e.Err }
