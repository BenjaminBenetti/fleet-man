//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

// holdRig is a running source "pod" of the harness whose agent can freeze a
// user cgroup (a plain dir that reads as frozen once asked), and a clone of
// it that holds it still by freezing.
type holdRig struct {
	h    *harness
	user string
}

func newHoldRig(t *testing.T) *holdRig {
	t.Helper()
	h := newHarness(t, PoolOptions{})
	target := h.addPod("src")
	root := h.roots[target.StatefulSet+"-0"]
	user := filepath.Join(root, "cg", podinit.UserCgroup)
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"cgroup.freeze": "0", "cgroup.events": "populated 1\nfrozen 1\n"} {
		if err := os.WriteFile(filepath.Join(user, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layout, _ := json.Marshal(podinit.Layout{V: 1, Container: filepath.Dir(user), Agent: filepath.Join(filepath.Dir(user), podinit.AgentCgroup), User: user})
	if err := os.MkdirAll(filepath.Join(root, "r"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "r", "cgroup.json"), layout, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sourceHolds.Delete(testCluster + "|" + target.StatefulSet) })
	return &holdRig{h: h, user: user}
}

// clone is a clone of the rig's source in the middle of its copy.
func (r *holdRig) clone(name string) *cloning {
	pods := r.h.pool
	var sts string
	for pod := range r.h.roots {
		sts = strings.TrimSuffix(pod, "-0")
	}
	set, pod := instanceObjects(r.h.ns, sts, "uid-src")
	want := expectFor(set, pod)
	c := &cloning{
		creation: &creation{p: pods, id: bound{cluster: testCluster, fleet: "fleet", name: name}, start: time.Now()},
		srcRec: InstanceRecord{Cluster: testCluster, Fleet: "fleet", Instance: "src", StatefulSet: sts, Gen: 1,
			StatefulSetUID: string(want.StatefulSetUID), SpecHash: want.SpecHash, ImageDigests: want.ImageDigests},
		src:     where{cluster: testCluster, sts: sts},
		running: true,
		plan:    ClonePlan{Tier: TierCopy, Hold: HoldFreeze},
	}
	c.jobCtx = context.Background()
	return c
}

func (r *holdRig) frozen(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.user, "cgroup.freeze"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data)) == "1"
}

func shortFreeze(t *testing.T, watchdog, renew time.Duration) {
	t.Helper()
	oldF, oldR := cloneFreeze, cloneFreezeRenew
	cloneFreeze, cloneFreezeRenew = watchdog, renew
	t.Cleanup(func() { cloneFreeze, cloneFreezeRenew = oldF, oldR })
}

// A freeze fleetd renews in time holds the source until the clone lets it
// go, and the clone is of one point in time.
func TestCloneFreezeRenewedInTimeHolds(t *testing.T) {
	shortFreeze(t, 600*time.Millisecond, 150*time.Millisecond)
	r := newHoldRig(t)
	c := r.clone("copy")
	if err := c.hold(context.Background(), fleet.StepSeed); err != nil {
		t.Fatal(err)
	}
	// A barrier sent again on a newer connection holds once.
	if err := c.hold(context.Background(), fleet.StepSeed); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !r.frozen(t) {
			t.Fatal("the source thawed while fleetd renewed its freeze")
		}
	}
	c.letGo()
	if r.frozen(t) {
		t.Fatal("letting the source go left it frozen")
	}
	if why := c.bestEffortWhy(); why != "" {
		t.Fatalf("a clone held still throughout is best-effort: %s", why)
	}
}

// A renewal that comes after the freeze's watchdog thawed the source never
// freezes it again (which would hide that it ran on): the clone is
// best-effort.
func TestCloneFreezeTheWatchdogEndedIsBestEffort(t *testing.T) {
	shortFreeze(t, 400*time.Millisecond, time.Second)
	r := newHoldRig(t)
	c := r.clone("copy")
	if err := c.hold(context.Background(), fleet.StepSeed); err != nil {
		t.Fatal(err)
	}
	if !r.frozen(t) {
		t.Fatal("the hold did not freeze the source")
	}
	// The watchdog thaws at 0.4s, the renewal comes at 1s; the copy ends
	// at 1.2s, before a freeze that renewal had made again would end.
	time.Sleep(1200 * time.Millisecond)
	if r.frozen(t) {
		t.Fatal("a late renewal froze the source again")
	}
	c.letGo()
	if why := c.bestEffortWhy(); !strings.Contains(why, "watchdog") {
		t.Fatalf("a clone whose freeze the watchdog ended: best-effort %q", why)
	}
	if w := c.cloneWarnings(); len(w) == 0 || !strings.HasPrefix(w[0], bestEffortPrefix) {
		t.Fatalf("banner %q", w)
	}
}

// A source found thawed when the clone lets it go (thawed by hand, or its
// pod restarted) ran on before the copy ended: the clone is best-effort.
func TestCloneOfASourceThawedMeanwhileIsBestEffort(t *testing.T) {
	shortFreeze(t, 5*time.Second, time.Second)
	r := newHoldRig(t)
	c := r.clone("copy")
	if err := c.hold(context.Background(), fleet.StepSeed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.user, "cgroup.freeze"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.letGo()
	if why := c.bestEffortWhy(); !strings.Contains(why, "no longer frozen") {
		t.Fatalf("a clone whose source thawed meanwhile: best-effort %q", why)
	}
}

// Two clones of one source hold it one at a time: the second waits for the
// first to let it go, whose thaw never ends the second's freeze.
func TestClonesOfOneSourceHoldItInTurn(t *testing.T) {
	shortFreeze(t, 5*time.Second, time.Second)
	r := newHoldRig(t)
	first, second := r.clone("one"), r.clone("two")
	if err := first.hold(context.Background(), fleet.StepSeed); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := second.hold(ctx, fleet.StepSeed); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second hold while the first is in force: %v", err)
	}
	// Nor does a clone read the source while another holds it.
	if _, err := second.takeSlot(ctx, fleet.StepSeed); err == nil {
		t.Fatal("the source's slot was free while a clone held it")
	}

	held := make(chan error, 1)
	go func() { held <- second.hold(context.Background(), fleet.StepSeed) }()
	time.Sleep(100 * time.Millisecond)
	first.letGo()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if !r.frozen(t) {
		t.Fatal("the second clone's hold did not freeze the source")
	}
	second.letGo()
	for _, c := range []*cloning{first, second} {
		if why := c.bestEffortWhy(); why != "" {
			t.Fatalf("%s is best-effort: %s", c.id.name, why)
		}
	}
	// Both let go: the slot is free.
	slot, err := first.takeSlot(context.Background(), fleet.StepSeed)
	if err != nil {
		t.Fatal(err)
	}
	<-slot
}
