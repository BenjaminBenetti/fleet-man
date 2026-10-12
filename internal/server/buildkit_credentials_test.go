package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
)

// credentialRig is a daemon whose instances on connection c1 hold shared
// Buildkit credentials (issuer), with the clock at at and each renewal
// answered by renew (nil: the instance gets issuer.Credential).
type credentialRig struct {
	issuer *svcruntimetest.Issuer
	at     time.Time
	renew  func(inst string) error

	mu      sync.Mutex
	renewed []string
}

func newCredentialRig(t *testing.T) *credentialRig {
	t.Helper()
	isolateFleetDir(t)
	running := func(name string) *fleet.Instance {
		return &fleet.Instance{Name: name, Status: fleet.StatusRunning, ContainerID: "k8s:c1/fleet/" + name + "@g1", Backend: fleet.BackendKubernetes, KubeCluster: "c1"}
	}
	stopped := running("k-stopped")
	stopped.Status, stopped.ContainerID = fleet.StatusStopped, ""
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: []*fleet.Instance{
			running("k1"), stopped, {Name: "d1", Status: fleet.StatusRunning, ContainerID: "abc", Backend: fleet.BackendDevcontainer},
		}},
		"beta": {Name: "beta", Settings: fleet.FleetSettings{DebCacheServer: true}, Instances: []*fleet.Instance{running("k1")}},
	})
	r := &credentialRig{issuer: svcruntimetest.NewIssuer("kubernetes:c1"), at: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	host := svcruntimetest.New("docker")
	stubServicesFor(t, func(inst *fleet.Instance) svcruntime.Runtime {
		if inst.KubeCluster == "c1" {
			return r.issuer
		}
		return host
	})
	origNow, origRenew := credentialNow, renewInstanceCred
	credentialNow = func() time.Time { return r.at }
	renewInstanceCred = func(rt svcruntime.Runtime, fleetName string, inst *fleet.Instance) error {
		if rt != r.issuer {
			t.Errorf("renewed through %v", rt)
		}
		r.mu.Lock()
		r.renewed = append(r.renewed, fleetName+"/"+inst.Name)
		renew := r.renew
		r.mu.Unlock()
		if renew != nil {
			if err := renew(inst.Name); err != nil {
				return err
			}
		}
		cred := r.issuer.Credential
		cred.NotBefore, cred.NotAfter = r.at, r.at.Add(7*24*time.Hour)
		r.issuer.SetInstalled(fleetName, inst.Name, cred)
		return nil
	}
	t.Cleanup(func() { credentialNow, renewInstanceCred = origNow, origRenew })
	return r
}

func (r *credentialRig) holds(fleetName, inst string, age, left time.Duration) {
	r.issuer.SetInstalled(fleetName, inst, fleet.ServiceCredential{NotBefore: r.at.Add(-age), NotAfter: r.at.Add(left)})
}

func (r *credentialRig) pass(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	r.renewed = nil
	r.mu.Unlock()
	renewSharedServiceCredentials(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.renewed...)
	sort.Strings(out)
	return out
}

func banner(t *testing.T, fleetName, inst string) string {
	t.Helper()
	data, err := os.ReadFile(state.WarnPath(fleetName, inst))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	return string(data)
}

// TestRenewSharedServiceCredentials: a running cluster instance of a fleet
// with shared Buildkit on is given a credential when it holds none and
// renewed once its one is a day old; no other instance is asked (stopped,
// docker, a fleet without Buildkit); one whose renewal keeps failing shows
// the expiry banner, which the next good renewal takes off; one the runtime
// no longer serves loses it.
func TestRenewSharedServiceCredentials(t *testing.T) {
	r := newCredentialRig(t)
	if got := r.pass(t); !reflect.DeepEqual(got, []string{"alpha/k1"}) {
		t.Fatalf("first pass renewed %v", got)
	}
	if got := r.pass(t); len(got) != 0 {
		t.Fatalf("a fresh credential was renewed again: %v", got)
	}
	r.holds("alpha", "k1", 23*time.Hour, 6*24*time.Hour)
	if got := r.pass(t); len(got) != 0 {
		t.Fatalf("a credential under a day old was renewed: %v", got)
	}
	r.holds("alpha", "k1", 24*time.Hour, 6*24*time.Hour)
	if got := r.pass(t); len(got) != 1 {
		t.Fatalf("a day-old credential was not renewed: %v", got)
	}
	if b := banner(t, "alpha", "k1"); b != "" {
		t.Fatalf("banner %q", b)
	}

	// Renewals failing for days: the banner counts down, then says it
	// expired.
	r.renew = func(string) error { return errors.New("the pod's agent is not reachable") }
	r.holds("alpha", "k1", 5*24*time.Hour, 40*time.Hour)
	r.pass(t)
	if b := banner(t, "alpha", "k1"); b != "Buildkit credential expires in 40h; it renews while the Fleet daemon runs" {
		t.Fatalf("banner %q", b)
	}
	r.at = r.at.Add(38*time.Hour + 30*time.Minute)
	r.pass(t)
	if b := banner(t, "alpha", "k1"); b != "Buildkit credential expires in 1h; it renews while the Fleet daemon runs" {
		t.Fatalf("banner %q", b)
	}
	r.at = r.at.Add(4 * time.Hour)
	r.pass(t)
	if b := banner(t, "alpha", "k1"); b != "Buildkit credential expired 2h ago; shared Buildkit builds fail until the Fleet daemon renews it" {
		t.Fatalf("banner %q", b)
	}

	// An unreachable connection is not asked; the banner stays.
	r.renew = nil
	r.issuer.Unreachable = true
	if got := r.pass(t); len(got) != 0 {
		t.Fatalf("renewed on an unreachable connection: %v", got)
	}
	r.issuer.Unreachable = false
	state.WriteWarn("alpha", "k1", "deb cache: failed\n"+credentialBanner(-2*time.Hour))
	r.pass(t)
	if b := banner(t, "alpha", "k1"); b != "deb cache: failed" {
		t.Fatalf("a renewed credential left banner %q", b)
	}

	// The connection stops serving the fleet's Buildkit: the banner goes.
	r.holds("alpha", "k1", 6*24*time.Hour, 20*time.Hour)
	state.WriteWarn("alpha", "k1", credentialBanner(20*time.Hour))
	r.renew = func(string) error {
		return fmt.Errorf("%w: shared Buildkit is not allowed on c1", svcruntime.ErrNoEndpoint)
	}
	r.pass(t)
	if b := banner(t, "alpha", "k1"); b != "" {
		t.Fatalf("banner %q on an instance the connection no longer serves", b)
	}
}

// TestRefusalChanged: a refusal is news once, until its reason changes or a
// renewal goes through.
func TestRefusalChanged(t *testing.T) {
	h := credentialHolder{fleet: "alpha", inst: &fleet.Instance{Name: "k1"}, rt: svcruntimetest.New("kubernetes:c9")}
	t.Cleanup(func() { credentialRefusals.Delete("kubernetes:c9/alpha/k1") })
	for i, step := range []struct {
		why  string
		news bool
	}{
		{"", false},
		{"not allowed", true},
		{"not allowed", false},
		{"admission denies it", true},
		{"", true},
		{"", false},
		{"not allowed", true},
	} {
		if got := refusalChanged(h, step.why); got != step.news {
			t.Fatalf("step %d (%q): changed = %v, want %v", i, step.why, got, step.news)
		}
	}
}

func TestCredentialBanner(t *testing.T) {
	for left, want := range map[time.Duration]string{
		7 * 24 * time.Hour:               "",
		48 * time.Hour:                   "",
		47*time.Hour + 59*time.Minute:    "Buildkit credential expires in 47h; it renews while the Fleet daemon runs",
		25 * time.Minute:                 "Buildkit credential expires in 25m; it renews while the Fleet daemon runs",
		10 * time.Second:                 "Buildkit credential expires in 1m; it renews while the Fleet daemon runs",
		0:                                "Buildkit credential expired 1m ago; shared Buildkit builds fail until the Fleet daemon renews it",
		-(3*time.Hour + 10*time.Minute):  "Buildkit credential expired 3h ago; shared Buildkit builds fail until the Fleet daemon renews it",
		-(8 * 24 * time.Hour):            "Buildkit credential expired 192h ago; shared Buildkit builds fail until the Fleet daemon renews it",
		credentialBannerWithin - 1:       "Buildkit credential expires in 47h; it renews while the Fleet daemon runs",
		credentialBannerWithin + 1:       "",
		-credentialRenewAge:              "Buildkit credential expired 24h ago; shared Buildkit builds fail until the Fleet daemon renews it",
		credentialRenewAge + time.Minute: "Buildkit credential expires in 24h; it renews while the Fleet daemon runs",
	} {
		if got := credentialBanner(left); got != want {
			t.Errorf("credentialBanner(%v) = %q, want %q", left, got, want)
		}
	}
}

// TestReensureClusterBuildkit: a change of a connection's Allow shared
// Buildkit re-ensures the shared Buildkit of each fleet with it on and an
// instance there (a refusal is no failure), and the credentials follow.
func TestReensureClusterBuildkit(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: []*fleet.Instance{{Name: "k1", Backend: fleet.BackendKubernetes, KubeCluster: "c1"}}},
		"beta":  {Name: "beta", Settings: fleet.FleetSettings{DebCacheServer: true}, Instances: []*fleet.Instance{{Name: "k1", Backend: fleet.BackendKubernetes, KubeCluster: "c1"}}},
		"gamma": {Name: "gamma", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: []*fleet.Instance{{Name: "k1", Backend: fleet.BackendKubernetes, KubeCluster: "c2"}}},
	})
	recs := clusterRecorders(t, svcruntimetest.New("docker"), nil, "c1", "c2")
	recs["c1"].FailOn("EnsureService", fmt.Errorf("%w: not allowed", svcruntime.ErrNoEndpoint))
	gave := stubGiveBuilder(t, nil)
	reensureClusterBuildkit(context.Background(), "c1")
	if got := ensured(recs["c1"]); !reflect.DeepEqual(got, []string{"alpha/buildkit"}) {
		t.Fatalf("c1 ensured %v", got)
	}
	if got := recs["c2"].Calls(); len(got) != 0 {
		t.Fatalf("c2 calls %+v", got)
	}
	if got := gave(); len(got) != 0 {
		t.Fatalf("a connection that refused the server gave builders to %v", got)
	}
}

// stubGiveBuilder records the instances given a builder (giveInstanceBuilder),
// failing those fail names.
func stubGiveBuilder(t *testing.T, fail map[string]error) func() []string {
	t.Helper()
	var mu sync.Mutex
	var gave []string
	orig := giveInstanceBuilder
	giveInstanceBuilder = func(_ svcruntime.Runtime, fleetName string, inst *fleet.Instance) (bool, error) {
		mu.Lock()
		gave = append(gave, fleetName+"/"+inst.Name)
		mu.Unlock()
		if err := fail[fleetName+"/"+inst.Name]; err != nil {
			return false, err
		}
		return true, nil
	}
	t.Cleanup(func() { giveInstanceBuilder = orig })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(gave)
		sort.Strings(out)
		return out
	}
}

// Buildkit allowed again on a connection: the running instances of the
// fleets whose server came back get the shared builder now, as a start
// gives it, not at their next start (they only had their credentials
// renewed); stopped ones, other connections' and fleets without Buildkit
// get nothing. A failure is a warning on the instance.
func TestReensureClusterBuildkitGivesRunningInstancesTheBuilder(t *testing.T) {
	isolateFleetDir(t)
	kube := func(name, cluster string, st fleet.InstanceStatus) *fleet.Instance {
		return &fleet.Instance{Name: name, Backend: fleet.BackendKubernetes, KubeCluster: cluster, Status: st, ContainerID: "kubernetes:" + cluster + "/" + name}
	}
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: []*fleet.Instance{
			kube("k1", "c1", fleet.StatusRunning), kube("k2", "c1", fleet.StatusRunning), kube("k3", "c1", fleet.StatusStopped), kube("k4", "c2", fleet.StatusRunning),
		}},
		"beta": {Name: "beta", Settings: fleet.FleetSettings{DebCacheServer: true}, Instances: []*fleet.Instance{kube("k1", "c1", fleet.StatusRunning)}},
	})
	clusterRecorders(t, svcruntimetest.New("docker"), nil, "c1", "c2")
	gave := stubGiveBuilder(t, map[string]error{"alpha/k2": errors.New("no buildx")})
	// k1 started while the connection refused the server.
	state.WriteWarn("alpha", "k1", "other warning\nbuildkit server: shared Buildkit is not allowed on c1")
	reensureClusterBuildkit(context.Background(), "c1")
	if got := gave(); !reflect.DeepEqual(got, []string{"alpha/k1", "alpha/k2"}) {
		t.Fatalf("builders given to %v; want alpha's running instances on c1", got)
	}
	if got := banner(t, "alpha", "k1"); got != "other warning" {
		t.Fatalf("alpha/k1's banner %q; want the refusal gone, the rest kept", got)
	}
	if got := banner(t, "alpha", "k2"); got != "buildkit server: no buildx" {
		t.Fatalf("alpha/k2's banner %q", got)
	}
}
