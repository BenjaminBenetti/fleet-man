//go:build !nok8s && unix

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The shared-state sync manager against stand-in pods: each "pod" runs its
// share's spoke in process, a sharesync side over a folder of its own, the
// way a pod's spoke keeps the share.

const syncCluster = "minikube-a1b2c3"

// syncRoots are a fleet's folders with Claude Code's mount and one custom
// mount, as an instance record keeps them.
var syncRoots = []podshare.SpokeRoot{
	{Name: ".claude"},
	{Name: ".mnt", Reserved: []string{"opt/data"}},
	{Name: "files", Reserved: []string{".claude.json", "claude-settings.local.json"}},
}

// fakeSpokes is the clusters' side: shares, pods and their spokes.
type fakeSpokes struct {
	t    *testing.T
	base string

	mu      sync.Mutex
	shares  map[string]kubernetes.ShareRecord
	running map[string]time.Time
	stops   map[string]chan struct{}
	served  []string
	retired []string
	// full are the share sides whose disk is full; deaf those that cannot
	// rescan.
	full map[string]bool
	deaf map[string]bool
}

func newFakeSpokes(t *testing.T) *fakeSpokes {
	return &fakeSpokes{
		t: t, base: t.TempDir(), shares: map[string]kubernetes.ShareRecord{}, running: map[string]time.Time{},
		stops: map[string]chan struct{}{}, full: map[string]bool{}, deaf: map[string]bool{},
	}
}

func (f *fakeSpokes) share(_, key string) (kubernetes.ShareRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shares[key]
	if !ok {
		return kubernetes.ShareRecord{}, kubernetes.ErrNoShare
	}
	return s, nil
}

func (f *fakeSpokes) elect(_ context.Context, _, _, key string, share kubernetes.ShareRecord) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if share.Claim == "" {
		if _, ok := f.running[key]; ok {
			return key, nil
		}
		return "", nil
	}
	best, bestAt := "", time.Time{}
	for k, at := range f.running {
		if f.shares[k].Claim == share.Claim && (best == "" || at.Before(bestAt)) {
			best, bestAt = k, at
		}
	}
	return best, nil
}

// dir is a share side's folder.
func (f *fakeSpokes) dir(side string) string {
	return filepath.Join(f.base, strings.NewReplacer(":", "_", "/", "_").Replace(side))
}

func (f *fakeSpokes) serve(ctx context.Context, _, key string, served []podshare.SpokeRoot, hub sharesync.RemoteHub, info sharesync.SideInfo, attached func()) error {
	f.mu.Lock()
	share := f.shares[key]
	if len(served) > 0 {
		share.Roots = served
	}
	stop := f.stops[key]
	_, up := f.running[key]
	f.served = append(f.served, key)
	f.mu.Unlock()
	if !up {
		return errors.New("the pod is not running")
	}
	dir := f.dir(info.ID)
	var roots []sharesync.TreeRoot
	for _, r := range share.Roots {
		folder := filepath.Join(dir, r.Name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return err
		}
		roots = append(roots, sharesync.TreeRoot{Name: r.Name, Dir: folder, Reserved: r.Reserved, Skip: r.Skip, Only: r.Only})
	}
	tree, err := sharesync.OpenTree(roots, sharesync.TreeOptions{
		StateDir: dir + ".state", FullReason: "share full", MinFree: 0.05, Settle: -1, MergeSettle: -1,
		Space: func(string) (sharesync.Space, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.full[info.ID] {
				return sharesync.Space{Total: 100 << 20, Free: 1 << 20}, nil
			}
			return sharesync.Space{Total: 100 << 30, Free: 50 << 30}, nil
		},
	})
	if err != nil {
		return err
	}
	defer tree.Close()
	side := sharesync.NewSide(info, tree, hub, sharesync.SideOptions{NoWatch: true, Poll: 20 * time.Millisecond, Debounce: time.Millisecond})
	var peer sharesync.Peer = side
	f.mu.Lock()
	if f.deaf[info.ID] {
		peer = deafPeer{side}
	}
	f.mu.Unlock()
	w, err := hub.Attach(ctx, info, peer, tree.Hello())
	if err != nil {
		return err
	}
	if err := tree.Welcome(w); err != nil {
		return err
	}
	attached()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = side.Run(runCtx)
	}()
	select {
	case <-ctx.Done():
	case <-stop:
	}
	cancel()
	<-done
	return errors.New("the pod went")
}

func (f *fakeSpokes) usage(_ context.Context, cluster, fleetName string) (fleet.ShareUsage, bool, error) {
	return fleet.ShareUsage{Cluster: cluster, Location: "minikube", Fleet: fleetName, Claim: "fs-" + fleetName + "-share", CapacityBytes: 10 << 30, UsedBytes: 1 << 30, Measured: true}, true, nil
}

func (f *fakeSpokes) retire(_ context.Context, cluster, fleetName string, keep bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired = append(f.retired, fmt.Sprintf("%s/%s keep=%v", cluster, fleetName, keep))
	for _, s := range f.shares {
		if s.Claim != "" {
			return s.Claim, nil
		}
	}
	return "", nil // the copy tier: no share
}

// up starts key's pod, down stops it.
func (f *fakeSpokes) up(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[key] = time.Now()
	f.stops[key] = make(chan struct{})
}

func (f *fakeSpokes) down(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, key)
	if ch := f.stops[key]; ch != nil {
		close(ch)
		delete(f.stops, key)
	}
}

func (f *fakeSpokes) servedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.served)
}

// deafPeer is a side whose rescans fail (a spoke cut off mid-flush).
type deafPeer struct{ *sharesync.Side }

func (deafPeer) Rescan(context.Context) error { return errors.New("the spoke does not answer") }

// syncRig is a manager on a temp HOME with a fleet and stand-in pods.
type syncRig struct {
	t      *testing.T
	m      *shareSync
	svc    *service
	spokes *fakeSpokes
	fleet  string
	cancel context.CancelFunc
}

func newSyncRig(t *testing.T) *syncRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prev := []time.Duration{shareSyncInterval, sharePublishInterval, shareReadyTimeout, shareStopFlush, shareDestroyFlush}
	shareSyncInterval, sharePublishInterval = 50*time.Millisecond, 20*time.Millisecond
	shareReadyTimeout, shareStopFlush, shareDestroyFlush = 10*time.Second, 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		shareSyncInterval, sharePublishInterval, shareReadyTimeout, shareStopFlush, shareDestroyFlush = prev[0], prev[1], prev[2], prev[3], prev[4]
	})
	svc := newService()
	ctx, cancel := context.WithCancel(context.Background())
	go svc.hub.run(ctx)
	r := &syncRig{t: t, svc: svc, spokes: newFakeSpokes(t), fleet: "f", cancel: cancel}
	r.m = svc.shareSync
	r.m.spokes = r.spokes
	r.m.records = func() []fleet.KubeCluster { return []fleet.KubeCluster{{ID: syncCluster, DisplayName: "minikube"}} }
	r.m.usable = func(context.Context, string) bool { return true }
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{r.fleet: {Name: r.fleet}}}); err != nil {
		t.Fatal(err)
	}
	go r.m.run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = r.m.flushAndClose(context.Background())
	})
	return r
}

// instance adds a cluster instance of the fleet whose share is claim's
// ("" for the copy tier: its own).
func (r *syncRig) instance(name, key, claim string, status fleet.InstanceStatus) {
	r.t.Helper()
	r.instanceOn(syncCluster, name, key, claim, syncRoots, status)
}

// instanceOn is instance on cluster, its share holding roots.
func (r *syncRig) instanceOn(cluster, name, key, claim string, roots []podshare.SpokeRoot, status fleet.InstanceStatus) {
	r.t.Helper()
	mode := "single-node"
	if claim == "" {
		mode = "copy"
	}
	r.instanceWith(cluster, name, key, kubernetes.ShareRecord{Mode: mode, Claim: claim, Roots: roots, UID: -1, GID: -1}, status)
}

// instanceWith is instanceOn with its share's record as given.
func (r *syncRig) instanceWith(cluster, name, key string, share kubernetes.ShareRecord, status fleet.InstanceStatus) {
	r.t.Helper()
	r.spokes.mu.Lock()
	r.spokes.shares[key] = share
	r.spokes.mu.Unlock()
	if err := state.Update(func(st *state.State) error {
		f := st.Fleets[r.fleet]
		f.Instances = append(f.Instances, &fleet.Instance{Name: name, Status: status, Backend: fleet.BackendKubernetes, KubeCluster: cluster, KubeKey: key, ContainerID: "k8s:" + key})
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *syncRig) setStatus(name string, status fleet.InstanceStatus) {
	r.t.Helper()
	if err := state.Update(func(st *state.State) error {
		inst, err := st.Fleets[r.fleet].GetInstance(name)
		if err != nil {
			return err
		}
		inst.Status = status
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// host is the path of rel in the fleet's folders on the daemon host.
func (r *syncRig) host(rel string) string {
	return filepath.Join(state.WorkspacesDir(), r.fleet, filepath.FromSlash(rel))
}

// share is the path of rel in a share side's folders.
func (r *syncRig) share(side, rel string) string {
	return filepath.Join(r.spokes.dir(side), filepath.FromSlash(rel))
}

func shareSide(claim string) string { return "k8s:" + syncCluster + "/" + claim }

func syncWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func syncRead(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

// hostLayout makes the folders the resolver makes on the host for the
// fleet: an empty .claude.json among them.
func (r *syncRig) hostLayout() {
	r.t.Helper()
	for _, d := range []string{".claude", ".mnt/opt/data", "files"} {
		if err := os.MkdirAll(r.host(d), 0o777); err != nil {
			r.t.Fatal(err)
		}
	}
	for _, f := range []string{"files/.claude.json", "files/claude-settings.local.json"} {
		syncWrite(r.t, r.host(f), "")
	}
}

func (r *syncRig) wait() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending := r.m.wait(ctx, r.fleet); len(pending) > 0 {
		r.t.Fatalf("not in sync: %v", pending)
	}
}

// TestShareSyncReadyGate: an instance coming up gets, before its hooks, the
// fleet's settings from the host and its cluster's login from that
// cluster's login store, never the host's own login; the host's empty
// .claude.json is seeded '{}' (never the share's real one replaced by it).
func TestShareSyncReadyGate(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	syncWrite(t, r.host(".claude/settings.json"), `{"model":"opus"}`)
	syncWrite(t, r.host(".claude/.credentials.json"), `{"token":"host"}`)
	syncWrite(t, filepath.Join(state.SyncLocationDir(r.fleet, syncCluster), ".claude", ".credentials.json"), `{"token":"cluster"}`)
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusCreating)
	r.spokes.up("aaaaaaaa")

	if err := r.m.ready(context.Background(), r.fleet, "alpha"); err != nil {
		t.Fatalf("ready: %v", err)
	}
	side := shareSide(claim)
	if got := syncRead(r.share(side, ".claude/settings.json")); got != `{"model":"opus"}` {
		t.Errorf("the pod's settings %q", got)
	}
	if got := syncRead(r.share(side, ".claude/.credentials.json")); got != `{"token":"cluster"}` {
		t.Errorf("the pod's login %q, want its cluster's", got)
	}
	if got := syncRead(r.host("files/.claude.json")); got != "{}" {
		t.Errorf("the host's empty .claude.json is %q, want the '{}' seed", got)
	}

	// An instance without fleet mounts, or not on a cluster, passes at once.
	if err := r.m.ready(context.Background(), r.fleet, "nobody"); err != nil {
		t.Errorf("ready of an unknown instance: %v", err)
	}
}

// TestShareSyncConvergesBothWaysAndKeepsLoginsPerLocation: `fleet sync
// wait` is a barrier both ways; a login made in a pod reaches its cluster's
// login store (and the automation scheduler hears of it), not the host; the
// pod's real .claude.json replaces the host's '{}' seed, never the reverse.
func TestShareSyncConvergesBothWaysAndKeepsLoginsPerLocation(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.wait()
	side := shareSide(claim)

	syncWrite(t, r.host(".mnt/opt/data/from-host.txt"), "host write")
	r.wait()
	if got := syncRead(r.share(side, ".mnt/opt/data/from-host.txt")); got != "host write" {
		t.Errorf("a host write in the pod after wait: %q", got)
	}
	syncWrite(t, r.share(side, ".mnt/opt/data/from-pod.txt"), "pod write")
	syncWrite(t, r.share(side, "files/.claude.json"), `{"numStartups":4}`)
	select {
	case <-r.svc.schedulerWake:
	default:
	}
	syncWrite(t, r.share(side, ".claude/.credentials.json"), `{"token":"pod"}`)
	r.wait()
	if got := syncRead(r.host(".mnt/opt/data/from-pod.txt")); got != "pod write" {
		t.Errorf("a pod's write on the host after wait: %q", got)
	}
	if got := syncRead(r.host("files/.claude.json")); got != `{"numStartups":4}` {
		t.Errorf("the host's .claude.json %q, want the pod's", got)
	}
	if got := syncRead(r.host(".claude/.credentials.json")); !strings.Contains(got, "no such file") {
		t.Errorf("the pod's login reached the host: %q", got)
	}
	if got := syncRead(filepath.Join(state.SyncLocationDir(r.fleet, syncCluster), ".claude/.credentials.json")); got != `{"token":"pod"}` {
		t.Errorf("the cluster's login store holds %q", got)
	}
	select {
	case <-r.svc.schedulerWake:
	default:
		t.Error("the scheduler did not hear of the login")
	}
	if loc := state.SyncLocationDir(r.fleet, syncCluster); !locationLoggedIn(r.fleet, syncCluster, fleet.LoginTools()[0]) {
		t.Errorf("the automation login check does not see the login in %s", loc)
	}
}

// TestShareSyncMovesTheSpokeWhenItsPodStops: a stop flushes, and the
// share's spoke moves to the fleet's other running pod.
func TestShareSyncMovesTheSpokeWhenItsPodStops(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.instance("beta", "bbbbbbbb", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	time.Sleep(5 * time.Millisecond)
	r.spokes.up("bbbbbbbb")
	r.wait()
	if got := r.spokes.servedKeys(); len(got) == 0 || got[0] != "aaaaaaaa" {
		t.Fatalf("served %v, want the oldest pod first", got)
	}
	side := shareSide(claim)
	syncWrite(t, r.share(side, ".claude/projects/p.jsonl"), "line\n")
	r.m.beforeStop(r.fleet, "alpha")
	if got := syncRead(r.host(".claude/projects/p.jsonl")); got != "line\n" {
		t.Errorf("after the stop's flush the host has %q", got)
	}
	r.spokes.down("aaaaaaaa")
	r.setStatus("alpha", fleet.StatusStopped)
	eventually(t, "the spoke to move to beta", func() bool {
		served := r.spokes.servedKeys()
		return served[len(served)-1] == "bbbbbbbb"
	})
	syncWrite(t, r.host(".claude/after.txt"), "x")
	r.wait()
	if got := syncRead(r.share(side, ".claude/after.txt")); got != "x" {
		t.Errorf("after the move the share has %q", got)
	}
}

// TestShareSyncDestroyFlushesThenRetiresTheShare: a fleet destroy flushes
// and has the share deleted; one whose flush fails has it kept (an orphan),
// with a warning that says so; a stop that could not flush is remembered.
func TestShareSyncDestroyFlushesThenRetiresTheShare(t *testing.T) {
	for _, tc := range []struct {
		name string
		deaf bool
		keep bool
	}{{"flushed", false, false}, {"flush failed", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSyncRig(t)
			r.hostLayout()
			claim := "fs-f-1a2b3c4d-share"
			r.spokes.deaf[shareSide(claim)] = tc.deaf
			r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
			r.spokes.up("aaaaaaaa")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.m.waitAttached(ctx, r.fleet)
			cancel()
			syncWrite(t, r.share(shareSide(claim), ".claude/last.txt"), "last")
			r.setStatus("alpha", fleet.StatusDeleting)
			ff := r.m.beforeDestroy(r.fleet, "", true)
			if ff.flushed == tc.keep {
				t.Fatalf("flushed = %v (%v)", ff.flushed, ff.err)
			}
			if !tc.keep && syncRead(r.host(".claude/last.txt")) != "last" {
				t.Errorf("the destroy's flush left the share's last change behind")
			}
			r.spokes.down("aaaaaaaa")
			warnings := r.m.retireFleet(r.fleet, ff)
			want := fmt.Sprintf("%s/f keep=%v", syncCluster, tc.keep)
			if got := r.spokes.retired; len(got) != 1 || got[0] != want {
				t.Errorf("retired %v, want %s", got, want)
			}
			if tc.keep != (len(warnings) == 1 && strings.Contains(warnings[0], "labelled an orphan")) {
				t.Errorf("warnings %q", warnings)
			}
			if r.m.fleetOf(r.fleet) != nil {
				t.Error("the fleet's sync is still open")
			}
		})
	}
}

// TestShareSyncRemembersAStopThatCouldNotFlush: a share whose stop could
// not flush is kept by the fleet's destroy even with no instance up.
func TestShareSyncRemembersAStopThatCouldNotFlush(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.spokes.deaf[shareSide(claim)] = true
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.m.beforeStop(r.fleet, "alpha")
	r.spokes.down("aaaaaaaa")
	r.setStatus("alpha", fleet.StatusStopped)
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[shareSide(claim)] {
		t.Fatalf("unflushed %v", got)
	}
	r.m.retireFleet(r.fleet, r.m.beforeDestroy(r.fleet, "", true))
	if got := r.spokes.retired; len(got) != 1 || !strings.HasSuffix(got[0], "keep=true") {
		t.Errorf("retired %v, want the share kept", got)
	}
}

// TestShareSyncAnEvacuationFlushesBeforeItsStop: moving a fleet off a node
// the cluster empties stops each instance as a Fleet stop does, its share's
// last changes flushed first: one that could not flush is remembered, and a
// destroy keeps it.
func TestShareSyncAnEvacuationFlushesBeforeItsStop(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.spokes.deaf[shareSide(claim)] = true
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	if err := kubernetes.SaveInstanceRecord(kubernetes.InstanceRecord{Cluster: syncCluster, Namespace: "ns", Key: "aaaaaaaa", Fleet: r.fleet, Instance: "alpha",
		StatefulSet: names.Instance(r.fleet, "alpha", "aaaaaaaa"), Gen: 1, Share: &kubernetes.ShareRecord{Mode: "single-node", Claim: claim}}); err != nil {
		t.Fatal(err)
	}
	r.spokes.up("aaaaaaaa")
	_, ms := r.svc.kubePods.evac.members(syncCluster, r.fleet)
	if len(ms) != 1 || ms[0].name != "alpha" {
		t.Fatalf("members %+v", ms)
	}
	// The pod itself is a stand-in: the backend's stop finds no cluster.
	_ = ms[0].stop()
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[shareSide(claim)] {
		t.Fatalf("unflushed %v, want the moved instance's share", got)
	}
}

// TestShareSyncStopCountsOnlyItsOwnClustersShare: a stop's flush that only
// another cluster's share held back (its spoke cut off) leaves the stopped
// instance's share flushed: a destroy deletes it, and keeps the other's.
func TestShareSyncStopCountsOnlyItsOwnClustersShare(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	const otherCluster = "kind-b4c5d6"
	claimA, claimB := "fs-f-1a2b3c4d-share", "fs-f-5e6f7a8b-share"
	sideB := "k8s:" + otherCluster + "/" + claimB
	r.spokes.deaf[sideB] = true
	r.instance("alpha", "aaaaaaaa", claimA, fleet.StatusRunning)
	r.instanceOn(otherCluster, "beta", "bbbbbbbb", claimB, syncRoots, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.spokes.up("bbbbbbbb")
	r.m.beforeStop(r.fleet, "alpha")
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); got[shareSide(claimA)] {
		t.Errorf("alpha's share marked unflushed by another cluster's: %v", got)
	}
	r.m.beforeStop(r.fleet, "beta")
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[sideB] {
		t.Errorf("beta's share, cut off, not marked unflushed: %v", got)
	}
}

// TestShareSyncForgetsAnUnflushedStopOnceFlushed: a share whose stop could
// not flush is no longer kept by a destroy once its spoke is back and a
// flush took everything it held.
func TestShareSyncForgetsAnUnflushedStopOnceFlushed(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	side := shareSide(claim)
	r.spokes.deaf[side] = true
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.m.beforeStop(r.fleet, "alpha")
	r.spokes.down("aaaaaaaa")
	r.setStatus("alpha", fleet.StatusStopped)
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[side] {
		t.Fatalf("unflushed %v", got)
	}

	// Started again, its spoke answering: a flush clears the mark.
	r.spokes.mu.Lock()
	r.spokes.deaf[side] = false
	r.spokes.mu.Unlock()
	r.setStatus("alpha", fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.wait()
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); got[side] {
		t.Errorf("still unflushed after a flush with the share attached: %v", got)
	}
	r.setStatus("alpha", fleet.StatusDeleting)
	ff := r.m.beforeDestroy(r.fleet, "", true)
	r.spokes.down("aaaaaaaa")
	r.m.retireFleet(r.fleet, ff)
	if got := r.spokes.retired; len(got) != 1 || !strings.HasSuffix(got[0], "keep=false") {
		t.Errorf("retired %v, want the flushed share deleted", got)
	}
}

// TestShareSyncAKeptShareStaysKept: a share a destroy kept (its flush
// failed) stays marked past the destroy, so the fleet made again under the
// name keeps it too when destroyed before a flush took what it holds; the
// marks of a cluster with no share there (the copy tier) go with it.
func TestShareSyncAKeptShareStaysKept(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.spokes.deaf[shareSide(claim)] = true
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.setStatus("alpha", fleet.StatusDeleting)
	ff := r.m.beforeDestroy(r.fleet, "", true)
	r.spokes.down("aaaaaaaa")
	r.m.retireFleet(r.fleet, ff)
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[shareSide(claim)] {
		t.Fatalf("the kept share is not marked past the destroy: %v", got)
	}

	// Made again; its first instance never came up (its pod never ran):
	// destroyed, its share is kept again.
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{r.fleet: {Name: r.fleet}}}); err != nil {
		t.Fatal(err)
	}
	r.instance("gamma", "cccccccc", claim, fleet.StatusDeleting)
	r.spokes.retired = nil
	r.m.retireFleet(r.fleet, r.m.beforeDestroy(r.fleet, "", true))
	if got := r.spokes.retired; len(got) != 1 || !strings.HasSuffix(got[0], "keep=true") {
		t.Errorf("retired %v, want the share kept again", got)
	}
}

// TestShareSyncDropsTheMarksOfACopyTierDestroyed: a copy-tier instance's
// unflushed mark does not outlive its fleet's destroy (its copy went with
// its disk), so it keeps no later fleet's share.
func TestShareSyncDropsTheMarksOfACopyTierDestroyed(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	side := "k8s:" + syncCluster + "/aaaaaaaa"
	r.spokes.deaf[side] = true
	r.instance("alpha", "aaaaaaaa", "", fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.m.beforeStop(r.fleet, "alpha")
	r.spokes.down("aaaaaaaa")
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); !got[side] {
		t.Fatalf("unflushed %v", got)
	}
	r.setStatus("alpha", fleet.StatusDeleting)
	r.m.retireFleet(r.fleet, r.m.beforeDestroy(r.fleet, "", true))
	if got := loadUnflushed(state.SyncFleetDir(r.fleet)); len(got) != 0 {
		t.Errorf("marks %v outlived the copy tier's destroy", got)
	}
}

// TestShareSyncFleetWithoutLogins: a fleet whose folders hold no logins
// (custom mounts alone) syncs without a login store, and opening one is no
// error.
func TestShareSyncFleetWithoutLogins(t *testing.T) {
	r := newSyncRig(t)
	roots := []podshare.SpokeRoot{{Name: ".mnt", Reserved: []string{"opt/data"}}}
	if err := os.MkdirAll(r.host(".mnt/opt/data"), 0o777); err != nil {
		t.Fatal(err)
	}
	claim := "fs-f-1a2b3c4d-share"
	r.instanceOn(syncCluster, "alpha", "aaaaaaaa", claim, roots, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	syncWrite(t, r.host(".mnt/opt/data/a.txt"), "a")
	r.wait()
	if got := syncRead(r.share(shareSide(claim), ".mnt/opt/data/a.txt")); got != "a" {
		t.Errorf("the share holds %q", got)
	}
	r.m.recMu.Lock()
	defer r.m.recMu.Unlock()
	fs := r.m.fleetOf(r.fleet)
	if fs == nil {
		t.Fatal("the fleet's sync is not open")
	}
	if len(fs.stores) != 0 {
		t.Errorf("login stores %v for a fleet without logins", fs.stores)
	}
	if store, err := r.m.openStore(fs, syncCluster, ""); store != nil || err != nil {
		t.Errorf("openStore without logins = %v, %v; want none and no error", store, err)
	}
}

// TestShareSyncRowsShowTheSyncState: a running instance's row is in sync
// once its share's spoke holds everything, and sync paused, naming why,
// while its share is full; a file waiting for room arrives once it has
// some.
func TestShareSyncRowsShowTheSyncState(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.wait()
	r.svc.hub.runtimeWanted.Store(true) // a TUI watches
	rowState := func() *fleetgrpc.ShareSyncStatus {
		got := make(chan *fleetgrpc.ShareSyncStatus, 1)
		r.svc.hub.post(func(h *hub) { got <- h.runtime[runtimeKey(r.fleet, "alpha")].GetShareSync() })
		return <-got
	}
	eventually(t, "the row in sync", func() bool {
		return rowState().GetState() == fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_IN_SYNC
	})
	side := shareSide(claim)
	r.spokes.mu.Lock()
	r.spokes.full[side] = true
	r.spokes.mu.Unlock()
	syncWrite(t, r.host(".mnt/opt/data/big.bin"), strings.Repeat("x", 2<<20))
	eventually(t, "the row paused", func() bool {
		s := rowState()
		return s.GetState() == fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED && s.GetReason() == "share full"
	})
	reports := r.m.report(context.Background(), r.fleet)
	if len(reports) != 1 || !slices.Contains(reports[0].Paused, "share full") || len(reports[0].Shares) != 1 {
		t.Errorf("sync status %+v", reports)
	}
	r.spokes.mu.Lock()
	r.spokes.full[side] = false
	r.spokes.mu.Unlock()
	r.m.kickCluster(syncCluster, r.fleet)
	r.wait()
	if fi, err := os.Stat(r.share(side, ".mnt/opt/data/big.bin")); err != nil || fi.Size() != 2<<20 {
		t.Errorf("the file that waited for room: %v %v", fi, err)
	}
	r.setStatus("alpha", fleet.StatusStopped)
	r.spokes.down("aaaaaaaa")
	eventually(t, "a stopped row without a sync state", func() bool { return rowState() == nil })
}

// TestShareSyncCopyTierSyncsEachInstance: on the copy tier each instance
// keeps its own copy, synced through its own spoke.
func TestShareSyncCopyTierSyncsEachInstance(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	r.instance("alpha", "aaaaaaaa", "", fleet.StatusRunning)
	r.instance("beta", "bbbbbbbb", "", fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.spokes.up("bbbbbbbb")
	syncWrite(t, r.host(".claude/CLAUDE.md"), "rules")
	r.wait()
	for _, key := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if got := syncRead(r.share("k8s:"+syncCluster+"/"+key, ".claude/CLAUDE.md")); got != "rules" {
			t.Errorf("%s's copy holds %q", key, got)
		}
	}
	syncWrite(t, r.share("k8s:"+syncCluster+"/aaaaaaaa", ".claude/from-a.md"), "a")
	r.wait()
	if got := syncRead(r.share("k8s:"+syncCluster+"/bbbbbbbb", ".claude/from-a.md")); got != "a" {
		t.Errorf("beta's copy holds %q of alpha's write", got)
	}
}

// TestShareSyncRPCs: the sync RPCs answer for the fleet, refuse an unknown
// one, and say a fleet without cluster instances has nothing to sync.
func TestShareSyncRPCs(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	ctx := context.Background()
	wait, err := r.svc.WaitShareSync(ctx, &fleetgrpc.WaitShareSyncRequest{Fleet: r.fleet, TimeoutMs: 10000})
	if err != nil || !wait.GetSynced() {
		t.Fatalf("WaitShareSync: %v %v", wait, err)
	}
	got, err := r.svc.GetShareSync(ctx, &fleetgrpc.GetShareSyncRequest{Fleet: r.fleet})
	if err != nil || len(got.GetFleets()) != 1 {
		t.Fatalf("GetShareSync: %v %v", got, err)
	}
	sides := map[string]bool{}
	for _, root := range got.GetFleets()[0].GetRoots() {
		for _, s := range root.GetSides() {
			sides[s.GetSide()] = true
		}
	}
	for _, want := range []string{"host", "store:" + syncCluster, shareSide(claim)} {
		if !sides[want] {
			t.Errorf("GetShareSync sides %v lack %s", sides, want)
		}
	}
	if _, err := r.svc.GetShareSync(ctx, &fleetgrpc.GetShareSyncRequest{Fleet: "nope"}); err == nil {
		t.Error("GetShareSync of a missing fleet")
	}
	conflicts, err := r.svc.ListShareConflicts(ctx, &fleetgrpc.ListShareConflictsRequest{Fleet: r.fleet})
	if err != nil || len(conflicts.GetConflicts()) != 0 {
		t.Errorf("ListShareConflicts: %v %v", conflicts, err)
	}

	// The spoke gone for good: waiting reports what is behind.
	r.spokes.deaf[shareSide(claim)] = true
	r.spokes.down("aaaaaaaa")
	r.spokes.up("aaaaaaaa")
	syncWrite(t, r.host(".claude/x.txt"), "x")
	wait, err = r.svc.WaitShareSync(ctx, &fleetgrpc.WaitShareSyncRequest{Fleet: r.fleet, TimeoutMs: 1500})
	if err != nil || wait.GetSynced() || len(wait.GetPending()) == 0 {
		t.Errorf("WaitShareSync with a deaf spoke: %v %v", wait, err)
	}
}

// TestFlushOutcomeKeepsOnlyTheSharesBehind: a destroy keeps the share of a
// cluster whose spoke did not flush (or whose stop could not), and every
// share when the host or a login store is behind.
func TestFlushOutcomeKeepsOnlyTheSharesBehind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		unflushed map[string]bool
		keepA     bool
		keepB     bool
	}{
		{"flushed", nil, nil, false, false},
		{"a spoke down", &sharesync.FlushError{Pending: []string{"k8s:a/fs-f-1-share: rescan: the spoke does not answer"}}, nil, true, false},
		{"a share not attached", &sharesync.FlushError{Pending: []string{"k8s:b/fs-f-2-share: its cluster cannot be reached now"}}, nil, false, true},
		{"the host behind", &sharesync.FlushError{Pending: []string{"host .claude: 3 to send"}}, nil, true, true},
		{"a root over its cap", &sharesync.FlushError{Pending: []string{".claude: 1 changes wait: .claude is over its 8 GiB cap"}}, nil, true, true},
		{"a stop that could not flush", nil, map[string]bool{"k8s:b/fs-f-2-share": true}, false, true},
		{"not a flush error", errors.New("the hub closed"), nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := flushOutcome(tc.err, tc.unflushed)
			if ff.keeps("a") != tc.keepA || ff.keeps("b") != tc.keepB {
				t.Errorf("keeps a=%v b=%v, want %v %v (%+v)", ff.keeps("a"), ff.keeps("b"), tc.keepA, tc.keepB, ff)
			}
			if ff.flushed != (!tc.keepA && !tc.keepB) || !ff.flushed && ff.err == nil {
				t.Errorf("flushed %v, err %v", ff.flushed, ff.err)
			}
		})
	}
}

// TestJobsWireTheSync: a create carries the Ready gate in its hooks; a stop
// flushes before its backend stops the pod; a fleet destroy flushes before
// the teardown and retires the share after it.
func TestJobsWireTheSync(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	claim := "fs-f-1a2b3c4d-share"
	r.instance("alpha", "aaaaaaaa", claim, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.wait()
	side := shareSide(claim)

	gated := make(chan error, 1)
	origCreate := jobRunCreate
	jobRunCreate = func(ctx context.Context, fleetName, instance, _, _ string, _ bool, _ fleet.BackendType) error {
		ready := backend.HooksFrom(ctx).SharedStateReady
		if ready == nil {
			gated <- errors.New("no Ready gate in the create's hooks")
			return nil
		}
		gated <- ready(ctx)
		return nil
	}
	t.Cleanup(func() { jobRunCreate = origCreate })
	j, err := r.svc.startCreateInstanceJob(&fleetgrpc.CreateInstanceRequest{Fleet: r.fleet, Instance: "gamma"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := awaitJob(context.Background(), j); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := <-gated; err != nil {
		t.Errorf("the create's gate: %v", err)
	}

	stopped := make(chan string, 1)
	origStop := jobRunStop
	jobRunStop = func(string, string) error {
		stopped <- syncRead(r.host(".claude/stop.txt"))
		return nil
	}
	t.Cleanup(func() { jobRunStop = origStop })
	syncWrite(t, r.share(side, ".claude/stop.txt"), "before the stop")
	j, err = r.svc.startStopInstanceJob(&fleetgrpc.StopInstanceRequest{Fleet: r.fleet, Instance: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := awaitJob(context.Background(), j); err != nil {
		t.Fatalf("stop job: %v", err)
	}
	if got := <-stopped; got != "before the stop" {
		t.Errorf("when the backend stopped the pod the host had %q", got)
	}

	origDown := jobDownInstance
	downed := make(chan string, 4)
	jobDownInstance = func(i *fleet.Instance) error {
		downed <- syncRead(r.host(".claude/destroy.txt"))
		r.spokes.down(i.KubeKey)
		return nil
	}
	t.Cleanup(func() { jobDownInstance = origDown })
	syncWrite(t, r.share(side, ".claude/destroy.txt"), "before the destroy")
	j, err = r.svc.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: r.fleet, DestroyFleet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := awaitJob(context.Background(), j); err != nil {
		t.Fatalf("destroy job: %v", err)
	}
	if got := <-downed; got != "before the destroy" {
		t.Errorf("when the instance was torn down the host had %q", got)
	}
	if got := r.spokes.retired; len(got) != 1 || got[0] != syncCluster+"/f keep=false" {
		t.Errorf("retired %v, want the flushed share deleted", got)
	}
}

// TestShareSyncDoesNotWaitForAShareNoPodRuns: a destroy of a fleet whose
// instance never got a pod (a failed create) flushes what there is at once:
// nothing on that share can change.
func TestShareSyncDoesNotWaitForAShareNoPodRuns(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	r.instance("alpha", "aaaaaaaa", "fs-f-1a2b3c4d-share", fleet.StatusDeleting)
	start := time.Now()
	ff := r.m.beforeDestroy(r.fleet, "", true)
	if !ff.flushed || time.Since(start) > 2*time.Second {
		t.Errorf("flushed %v (%v) after %v", ff.flushed, ff.err, time.Since(start))
	}
}
