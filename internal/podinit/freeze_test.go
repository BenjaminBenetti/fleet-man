package podinit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freezerFS is a cgroupfs whose user cgroup freezes the way the kernel's
// does: cgroup.events reports frozen once cgroup.freeze is 1, unless stuck
// (a process in uninterruptible sleep that never stops).
type freezerFS struct {
	*fakeCgroupFS
	user  string
	stuck bool
}

func (f *freezerFS) WriteFile(name string, data []byte) error {
	if err := f.fakeCgroupFS.WriteFile(name, data); err != nil {
		return err
	}
	if name == f.user+"/cgroup.freeze" {
		frozen := strings.TrimSpace(string(data)) == "1" && !f.stuck
		v := "populated 1\nfrozen 0\n"
		if frozen {
			v = "populated 1\nfrozen 1\n"
		}
		f.files[f.user+"/cgroup.events"] = v
	}
	return nil
}

func freezeLayout(t *testing.T, stuck bool) (*Layout, *freezerFS, string) {
	t.Helper()
	dir := "/sys/fs/cgroup/pod/ctr"
	cfs := newFakeCgroupFS(dir, "cpu memory pids", "max")
	user := dir + "/" + UserCgroup
	cfs.dirs[user] = true
	cfs.files[user+"/cgroup.freeze"] = "0\n"
	cfs.files[user+"/cgroup.events"] = "populated 1\nfrozen 0\n"
	ffs := &freezerFS{fakeCgroupFS: cfs, user: user, stuck: stuck}
	l := &Layout{V: 1, Container: dir, Agent: dir + "/" + AgentCgroup, User: user, fs: ffs}
	return l, ffs, filepath.Join(t.TempDir(), "freeze.json")
}

func TestFreezeRecordsItselfThenFreezes(t *testing.T) {
	l, ffs, marker := freezeLayout(t, false)
	now := time.Now()
	res, err := l.Freeze(marker, now, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deadline.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("deadline %v", res.Deadline)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "1" {
		t.Fatalf("cgroup.freeze = %q", got)
	}
	m, err := readMarker(marker)
	if err != nil || !m.Deadline.Equal(now.Add(30*time.Second)) || m.ThawedBy != "" {
		t.Fatalf("marker %+v, %v", m, err)
	}

	th, err := l.Thaw(marker, "")
	if err != nil || !th.WasFrozen || th.ByWatchdog != "" {
		t.Fatalf("thaw %+v, %v", th, err)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("cgroup.freeze after the thaw = %q", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the thaw left the freeze's record: %v", err)
	}
}

// A layout of today's pod-init freezes the user tree, fleet-user, so the
// containers a docker-in-docker engine runs (in fleet-user/docker) stop with
// the user's processes (in fleet-user/procs): a "consistent" clone's copy
// sees none of them write. Freezing procs alone left them running.
func TestFreezeTakesTheUserTreeWithItsDockerContainers(t *testing.T) {
	dir := "/sys/fs/cgroup/pod/ctr"
	cfs := newFakeCgroupFS(dir, "cpu memory pids", "max")
	tree := dir + "/" + UserCgroup
	procs, docker := tree+"/"+UserProcsCgroup, tree+"/"+UserDockerCgroup
	for _, d := range []string{tree, procs, docker} {
		cfs.dirs[d] = true
		cfs.files[d+"/cgroup.freeze"] = "0\n"
		cfs.files[d+"/cgroup.events"] = "populated 1\nfrozen 0\n"
	}
	ffs := &freezerFS{fakeCgroupFS: cfs, user: tree}
	l := &Layout{V: LayoutVersion, Container: dir, Agent: dir + "/" + AgentCgroup, User: procs, UserTree: tree, Docker: docker, fs: ffs}
	marker := filepath.Join(t.TempDir(), "freeze.json")
	now := time.Now()
	if _, err := l.Freeze(marker, now, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ffs.files[tree+"/cgroup.freeze"]; got != "1" {
		t.Fatalf("%s/cgroup.freeze = %q; want the user tree frozen", tree, got)
	}
	if got := ffs.files[procs+"/cgroup.freeze"]; got != "0\n" {
		t.Fatalf("procs' own cgroup.freeze = %q; the tree's freeze covers it", got)
	}
	if _, err := l.Renew(marker, now.Add(time.Minute)); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if th, err := l.Thaw(marker, ""); err != nil || !th.WasFrozen {
		t.Fatalf("thaw %+v, %v", th, err)
	}
	if got := ffs.files[tree+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("%s/cgroup.freeze after the thaw = %q", tree, got)
	}
	// A layout with a relative tree is no layout to freeze.
	if _, err := (&Layout{User: procs, UserTree: "fleet-user", fs: ffs}).Freeze(marker, now, now); !errors.Is(err, ErrNoLayout) {
		t.Fatalf("relative user tree: %v", err)
	}
}

func TestAFreezeThatDoesNotSettleIsThawed(t *testing.T) {
	old := freezeSettle
	freezeSettle = 50 * time.Millisecond
	t.Cleanup(func() { freezeSettle = old })
	l, ffs, marker := freezeLayout(t, true)
	if _, err := l.Freeze(marker, time.Now(), time.Now().Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "did not all stop") {
		t.Fatalf("a freeze that never settles: %v", err)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("the unsettled freeze was left in place: cgroup.freeze = %q", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its record stayed: %v", err)
	}
}

func TestNoLayoutCannotFreeze(t *testing.T) {
	var none *Layout
	if _, err := none.Freeze(filepath.Join(t.TempDir(), "m"), time.Now(), time.Now()); !errors.Is(err, ErrNoLayout) {
		t.Fatalf("freeze without a layout: %v", err)
	}
	if _, err := (&Layout{}).Thaw("", ""); !errors.Is(err, ErrNoLayout) {
		t.Fatalf("thaw without a layout: %v", err)
	}
}

func TestTheWatchdogThawsPastTheDeadlineOnly(t *testing.T) {
	l, ffs, marker := freezeLayout(t, false)
	now := time.Now()
	if _, err := l.Freeze(marker, now, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if note := l.WatchFreeze(marker, now.Add(31*time.Second)); note != "" {
		t.Fatalf("thawed within the grace the agent's timer has: %s", note)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "1" {
		t.Fatal("thawed before the deadline's grace")
	}
	note := l.WatchFreeze(marker, now.Add(40*time.Second))
	if !strings.Contains(note, "after its deadline") {
		t.Fatalf("note %q", note)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("not thawed past the deadline: %q", got)
	}
	// The thaw that comes after learns the watchdog was first.
	th, err := l.Thaw(marker, "")
	if err != nil || th.WasFrozen || th.ByWatchdog != "pod-init" {
		t.Fatalf("the late thaw: %+v, %v", th, err)
	}
	if note := l.WatchFreeze(marker, now.Add(time.Hour)); note != "" {
		t.Fatalf("nothing frozen, yet %q", note)
	}
}

func TestTheWatchdogThawsAnUnrecordedFreeze(t *testing.T) {
	l, ffs, marker := freezeLayout(t, false)
	if note := l.WatchFreeze(marker, time.Now()); note != "" {
		t.Fatalf("nothing frozen, yet %q", note)
	}
	ffs.files[ffs.user+"/cgroup.freeze"] = "1"
	if note := l.WatchFreeze(marker, time.Now()); !strings.Contains(note, "no record") {
		t.Fatalf("note %q", note)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("an unrecorded freeze stays: %q", got)
	}
}

func TestTheAgentsTimerIsReportedToo(t *testing.T) {
	l, _, marker := freezeLayout(t, false)
	now := time.Now()
	if _, err := l.Freeze(marker, now, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if res, err := l.Thaw(marker, "agent"); err != nil || !res.WasFrozen {
		t.Fatalf("the agent's timer: %+v, %v", res, err)
	}
	th, err := l.Thaw(marker, "")
	if err != nil || th.ByWatchdog != "agent" || th.WasFrozen {
		t.Fatalf("thaw after the agent's timer: %+v, %v", th, err)
	}
}

// A renewal moves the deadline of the freeze in force and never freezes
// again: one a watchdog ended (or that something thawed) is ErrThawed, and
// the watchdog's thaw is still reported to the thaw that comes after.
func TestARenewalNeverFreezesAgain(t *testing.T) {
	l, ffs, marker := freezeLayout(t, false)
	if _, err := l.Renew(marker, time.Now().Add(time.Minute)); !errors.Is(err, ErrThawed) {
		t.Fatalf("a renewal with no freeze: %v", err)
	}
	now := time.Now()
	if _, err := l.Freeze(marker, now, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	res, err := l.Renew(marker, now.Add(60*time.Second))
	if err != nil || !res.Deadline.Equal(now.Add(60*time.Second)) || res.FrozenAt.IsZero() {
		t.Fatalf("renewal %+v, %v", res, err)
	}
	if m, _ := readMarker(marker); !m.Deadline.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("the record keeps deadline %v", m.Deadline)
	}
	// Past the old deadline the freeze holds; past the new one the
	// watchdog thaws it, and a late renewal leaves it thawed.
	if note := l.WatchFreeze(marker, now.Add(40*time.Second)); note != "" {
		t.Fatalf("thawed before the renewed deadline: %s", note)
	}
	if note := l.WatchFreeze(marker, now.Add(70*time.Second)); note == "" {
		t.Fatal("not thawed past the renewed deadline")
	}
	if _, err := l.Renew(marker, now.Add(100*time.Second)); !errors.Is(err, ErrThawed) {
		t.Fatalf("a renewal after the watchdog's thaw: %v", err)
	}
	if got := ffs.files[ffs.user+"/cgroup.freeze"]; got != "0" {
		t.Fatalf("a late renewal froze again: cgroup.freeze = %q", got)
	}
	if th, err := l.Thaw(marker, ""); err != nil || th.ByWatchdog != "pod-init" {
		t.Fatalf("the thaw after: %+v, %v", th, err)
	}

	// Recorded, but no longer frozen (thawed by hand): no renewal either.
	if _, err := l.Freeze(marker, now, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	ffs.files[ffs.user+"/cgroup.freeze"] = "0"
	if _, err := l.Renew(marker, now.Add(60*time.Second)); !errors.Is(err, ErrThawed) {
		t.Fatalf("a renewal of a thawed cgroup: %v", err)
	}
}
