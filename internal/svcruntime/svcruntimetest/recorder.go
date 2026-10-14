// Package svcruntimetest provides a recording svcruntime.Runtime, so tests of
// the code that drives a fleet's shared services run without a docker daemon.
// Only test code imports it.
package svcruntimetest

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

var _ svcruntime.Runtime = (*Recorder)(nil)

// Call is one recorded Runtime call. Service, ContainerID and Instance are
// the arguments the method took ("" for one it did not).
type Call struct {
	Method      string
	Fleet       string
	Service     fleet.SharedService
	ContainerID string
	Instance    string
}

// Recorder is a svcruntime.Runtime that records every call that acts (Key,
// Endpoint, ReconfigureOnStart and ReconcileBudget only answer) and does
// nothing else. Set its fields before
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
	// Endpoints is what Endpoint returns for each service; a service with
	// none answers ErrNoEndpoint.
	Endpoints map[fleet.SharedService]fleet.ServiceEndpoint
	// Budget is ReconcileBudget's answer (a minute when zero).
	Budget time.Duration
	// OnCall, when set, runs on each recorded call before it returns (to hold
	// one in flight, say). It runs without the Recorder's lock.
	OnCall func(Call)

	mu       sync.Mutex
	calls    []Call
	failures map[string]error
}

// New returns a Recorder keyed name that reconfigures on start and has an
// endpoint for every service, as the docker runtime does (buildkit through a
// mount, the caches at "<name>-<service>" hosts).
func New(name string) *Recorder {
	return &Recorder{Name: name, Reconfigure: true, Endpoints: map[fleet.SharedService]fleet.ServiceEndpoint{
		fleet.ServiceBuildkit:   {},
		fleet.ServiceDebCache:   {URL: "http://" + name + "-debcache:3142", HostPort: name + "-debcache:3142"},
		fleet.ServiceImageCache: {URL: "http://" + name + "-imagecache:5000", HostPort: name + "-imagecache:5000"},
	}}
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

// PrepareFleet records the call.
func (r *Recorder) PrepareFleet(fleetName string) error {
	return r.record(Call{Method: "PrepareFleet", Fleet: fleetName})
}

// EnsureService records the call.
func (r *Recorder) EnsureService(fleetName string, svc fleet.SharedService) error {
	return r.record(Call{Method: "EnsureService", Fleet: fleetName, Service: svc})
}

// InstanceMounts records the call and returns Mounts[svc].
func (r *Recorder) InstanceMounts(fleetName string, svc fleet.SharedService) []backend.Mount {
	_ = r.record(Call{Method: "InstanceMounts", Fleet: fleetName, Service: svc})
	return r.Mounts[svc]
}

// Endpoint returns Endpoints[svc], or ErrNoEndpoint. It is not recorded: it
// only answers.
func (r *Recorder) Endpoint(_ string, svc fleet.SharedService) (fleet.ServiceEndpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.Endpoints[svc]; ok {
		return e, r.failures["Endpoint"]
	}
	return fleet.ServiceEndpoint{}, svcruntime.ErrNoEndpoint
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

// ReleaseFleet records the call.
func (r *Recorder) ReleaseFleet(fleetName string) error {
	return r.record(Call{Method: "ReleaseFleet", Fleet: fleetName})
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

// ReconcileBudget returns Budget (a minute when zero).
func (r *Recorder) ReconcileBudget() time.Duration {
	if r.Budget == 0 {
		return time.Minute
	}
	return r.Budget
}

var _ svcruntime.CredentialIssuer = (*Issuer)(nil)

// Issuer is a Recorder whose runtime authenticates each instance to its
// shared Buildkit (svcruntime.CredentialIssuer), as a cluster's does: it
// records IssueCredential and CredentialInstalled (InstalledCredential only
// answers) and remembers what was installed.
type Issuer struct {
	*Recorder
	// Credential is what IssueCredential returns.
	Credential fleet.ServiceCredential

	imu       sync.Mutex
	installed map[string]fleet.ServiceCredential
}

// NewIssuer returns an Issuer keyed name that, like a cluster's runtime,
// does not reconfigure on start and points instances at buildkit over TCP
// ("tcp://<name>-buildkit:1234", the name's ':' and '/' as '-'), with a
// stand-in credential for that host valid for a week from now.
func NewIssuer(name string) *Issuer {
	r := New(name)
	r.Reconfigure = false
	host := strings.NewReplacer(":", "-", "/", "-").Replace(name) + "-buildkit"
	r.Endpoints[fleet.ServiceBuildkit] = fleet.ServiceEndpoint{URL: "tcp://" + host + ":1234", HostPort: host + ":1234"}
	now := time.Now()
	return &Issuer{Recorder: r, Credential: fleet.ServiceCredential{
		CA: []byte("ca\n"), Cert: []byte("cert\n"), Key: []byte("key\n"), ServerName: host,
		Serial: "01", NotBefore: now, NotAfter: now.Add(7 * 24 * time.Hour),
	}}
}

// IssueCredential records the call and returns Credential.
func (r *Issuer) IssueCredential(fleetName, instanceName string, svc fleet.SharedService) (fleet.ServiceCredential, error) {
	err := r.record(Call{Method: "IssueCredential", Fleet: fleetName, Service: svc, Instance: instanceName})
	r.imu.Lock()
	defer r.imu.Unlock()
	return r.Credential, err
}

// CredentialInstalled records the call and remembers cred.
func (r *Issuer) CredentialInstalled(fleetName, instanceName string, svc fleet.SharedService, cred fleet.ServiceCredential) error {
	if err := r.record(Call{Method: "CredentialInstalled", Fleet: fleetName, Service: svc, Instance: instanceName}); err != nil {
		return err
	}
	r.SetInstalled(fleetName, instanceName, cred)
	return nil
}

// SetInstalled makes cred what instanceName holds.
func (r *Issuer) SetInstalled(fleetName, instanceName string, cred fleet.ServiceCredential) {
	r.imu.Lock()
	defer r.imu.Unlock()
	if r.installed == nil {
		r.installed = map[string]fleet.ServiceCredential{}
	}
	r.installed[fleetName+"/"+instanceName] = cred
}

// InstalledCredential answers what CredentialInstalled or SetInstalled
// remembered. It is not recorded.
func (r *Issuer) InstalledCredential(fleetName, instanceName string, _ fleet.SharedService) (time.Time, time.Time, bool) {
	r.imu.Lock()
	defer r.imu.Unlock()
	c, ok := r.installed[fleetName+"/"+instanceName]
	return c.NotBefore, c.NotAfter, ok
}
