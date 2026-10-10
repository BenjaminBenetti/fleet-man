// Package svcruntimetest provides a recording svcruntime.Runtime, so tests of
// the code that drives a fleet's shared services run without a docker daemon.
// Only test code imports it.
package svcruntimetest

import (
	"context"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

var _ svcruntime.Runtime = (*Recorder)(nil)

// Call is one recorded Runtime call. Service and ContainerID are the arguments
// the method took ("" for one it did not).
type Call struct {
	Method      string
	Fleet       string
	Service     fleet.SharedService
	ContainerID string
}

// Recorder is a svcruntime.Runtime that records every call that acts (Key and
// ReconfigureOnStart only answer) and does nothing else. Set its fields before
// sharing it between goroutines.
type Recorder struct {
	// Name is Key's answer.
	Name string
	// Reconfigure is ReconfigureOnStart's answer.
	Reconfigure bool
	// Unreachable makes Reachable report false.
	Unreachable bool
	// Mounts is what InstanceMounts returns for each service.
	Mounts map[fleet.SharedService][]backend.Mount
	// OnCall, when set, runs on each recorded call before it returns (to hold
	// one in flight, say). It runs without the Recorder's lock.
	OnCall func(Call)

	mu       sync.Mutex
	calls    []Call
	failures map[string]error
}

// New returns a Recorder keyed name that reconfigures on start, as the docker
// runtime does.
func New(name string) *Recorder {
	return &Recorder{Name: name, Reconfigure: true}
}

// FailOn makes every later call of method return err (nil clears it).
func (r *Recorder) FailOn(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures = map[string]error{}
	}
	r.failures[method] = err
}

// Calls returns the calls recorded so far, in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Reset forgets the calls recorded so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

func (r *Recorder) record(c Call) error {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	err := r.failures[c.Method]
	hook := r.OnCall
	r.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	return err
}

// Key returns Name.
func (r *Recorder) Key() string { return r.Name }

// EnsureService records the call.
func (r *Recorder) EnsureService(fleetName string, svc fleet.SharedService) error {
	return r.record(Call{Method: "EnsureService", Fleet: fleetName, Service: svc})
}

// InstanceMounts records the call and returns Mounts[svc].
func (r *Recorder) InstanceMounts(fleetName string, svc fleet.SharedService) []backend.Mount {
	_ = r.record(Call{Method: "InstanceMounts", Fleet: fleetName, Service: svc})
	return r.Mounts[svc]
}

// ConnectInstance records the call.
func (r *Recorder) ConnectInstance(fleetName, containerID string) error {
	return r.record(Call{Method: "ConnectInstance", Fleet: fleetName, ContainerID: containerID})
}

// StopService records the call.
func (r *Recorder) StopService(fleetName string, svc fleet.SharedService) error {
	return r.record(Call{Method: "StopService", Fleet: fleetName, Service: svc})
}

// WipeService records the call.
func (r *Recorder) WipeService(fleetName string, svc fleet.SharedService) error {
	return r.record(Call{Method: "WipeService", Fleet: fleetName, Service: svc})
}

// TeardownFleet records the call.
func (r *Recorder) TeardownFleet(fleetName string) error {
	return r.record(Call{Method: "TeardownFleet", Fleet: fleetName})
}

// ReconfigureOnStart returns Reconfigure.
func (r *Recorder) ReconfigureOnStart() bool { return r.Reconfigure }

// Reachable records the call and reports !Unreachable.
func (r *Recorder) Reachable(context.Context) bool {
	_ = r.record(Call{Method: "Reachable"})
	return !r.Unreachable
}
