//go:build unix

package sharesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A location's login store on the daemon host (SideInfo.LoginsOnly) keeps
// that location's logins and nothing else: the hub sends it no settings, no
// folders, no other location's login, and takes nothing else from it; a
// logout there reaches it as a delete; a hub that lost its state takes the
// login back from it.
func TestALoginStoreKeepsOnlyItsLocationsLogins(t *testing.T) {
	r := mergeRig(t, 0,
		SideInfo{ID: "host", Location: "host"},
		SideInfo{ID: "share-a", Location: "a"},
		SideInfo{ID: "share-b", Location: "b"},
	)
	r.addSide(SideInfo{ID: "store-a", Location: "a", LoginsOnly: true})
	r.write("share-a", "tool", ".credentials.json", `{"token":"a1"}`)
	r.write("share-b", "tool", ".credentials.json", `{"token":"b1"}`)
	r.write("host", "tool", ".credentials.json", `{"token":"host"}`)
	r.write("host", "tool", "settings.json", `{"theme":"dark"}`)
	r.write("host", "tool", "projects/p/s.jsonl", "line\n")
	r.write("host", "files", ".claude.json", `{"numStartups":3}`)
	r.sync()

	if got := r.read("store-a", "tool", ".credentials.json"); got != `{"token":"a1"}` {
		t.Errorf("the store of location a holds login %q, want a's", got)
	}
	for _, p := range []struct{ root, rel string }{{"tool", "settings.json"}, {"tool", "projects/p/s.jsonl"}, {"tool", "projects"}, {"files", ".claude.json"}} {
		if _, err := os.Lstat(r.path("store-a", p.root, p.rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the store holds %s/%s: %v", p.root, p.rel, err)
		}
	}
	if got := r.read("share-a", "tool", "settings.json"); got != `{"theme":"dark"}` {
		t.Errorf("a share's settings %q: the store held the sync back", got)
	}

	// What the store holds besides logins goes nowhere.
	r.write("store-a", "tool", "stray.txt", "only here")
	r.sync()
	if got := r.read("host", "tool", "stray.txt"); got != "<missing>" {
		t.Errorf("a file in the store reached the host: %q", got)
	}

	// A refresh and a logout in the location reach it.
	r.write("share-a", "tool", ".credentials.json", `{"token":"a2"}`)
	r.sync()
	if got := r.read("store-a", "tool", ".credentials.json"); got != `{"token":"a2"}` {
		t.Errorf("after a refresh the store holds %q", got)
	}
	if err := os.Remove(r.path("share-a", "tool", ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	r.sync()
	if got := r.read("store-a", "tool", ".credentials.json"); got != "<missing>" {
		t.Errorf("after a logout the store holds %q", got)
	}

	// Logged in again; then the hub starts over without its state, and a
	// fresh share of the location (made again) gets the store's login.
	r.write("share-a", "tool", ".credentials.json", `{"token":"a3"}`)
	r.sync()
	for _, id := range r.order {
		r.sides[id].stop()
	}
	r.hub.Close()
	for _, id := range r.order {
		r.sides[id].tree.Close()
	}
	if err := os.RemoveAll(r.base + "/hub"); err != nil {
		t.Fatal(err)
	}
	r.openHub()
	store := r.sides["store-a"]
	r.openSide(store)
	r.openSide(r.sides["host"])
	fresh := r.addSide(SideInfo{ID: "share-a2", Location: "a"})
	r.order = []string{"host", "store-a", "share-a2"}
	r.sync()
	if got := r.read(fresh.info.ID, "tool", ".credentials.json"); got != `{"token":"a3"}` {
		t.Errorf("a share made again got login %q from the store, want a3", got)
	}
	if got := r.read("host", "tool", ".credentials.json"); got != `{"token":"host"}` {
		t.Errorf("the host's own login is %q", got)
	}
}

// FlushSide waits for what one side is owed of the paths asked for, not for
// a large file still on its way, nor for another side.
func TestFlushSideWaitsForOneSidesPaths(t *testing.T) {
	r := mergeRig(t, time.Hour, SideInfo{ID: "host", Location: "host"}, SideInfo{ID: "pod", Location: "a"})
	slow := r.faulty(SideInfo{ID: "other", Location: "b"})
	slow.set(func(f *faultyPeer) { f.deaf = true })
	r.write("host", "tool", "settings.json", `{"theme":"dark"}`)
	r.write("host", "files", ".claude.json", `{"numStartups":1}`)
	r.write("host", "tool", "projects/big.bin", strings.Repeat("x", 4096))
	hot := func(root, p string) bool { return p == "settings.json" || p == ".claude.json" }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.hub.FlushSide(ctx, "pod", hot); err != nil {
		t.Fatalf("FlushSide: %v", err)
	}
	if got := r.read("pod", "tool", "settings.json"); got != `{"theme":"dark"}` {
		t.Errorf("the pod's settings %q", got)
	}
	if got := r.read("pod", "files", ".claude.json"); got != `{"numStartups":1}` {
		t.Errorf("the pod's .claude.json %q", got)
	}
	if err := r.hub.FlushSide(ctx, "nobody", hot); !errors.Is(err, ErrNotAttached) {
		t.Errorf("FlushSide of a side not attached: %v", err)
	}

	// A side whose rescan fails fails a flush at once, not at its deadline.
	slow.set(func(f *faultyPeer) { f.down = true })
	start := time.Now()
	err := r.flushErr(5 * time.Second)
	var fe *FlushError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "other: rescan") {
		t.Errorf("Flush with a side down: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Flush with a side down took %v", time.Since(start))
	}
	// …but not FlushSide of another side.
	if err := r.hub.FlushSide(ctx, "pod", nil); err != nil && strings.Contains(err.Error(), "other: rescan") {
		t.Errorf("FlushSide of the pod counted another side's rescan: %v", err)
	}
}

// FlushSide waits for what the side was owed when it began, not for later
// changes of the same path: ~/.claude.json, which every running Claude Code
// rewrites, would keep a flush chasing it until its deadline. Changes on
// either side are covered: another side's (a newer state to send) and the
// flushed side's own (its report merged, the merge to send back).
func TestFlushSideDoesNotChaseLaterChanges(t *testing.T) {
	for _, churner := range []string{"host", "pod"} {
		t.Run(churner+" keeps writing", func(t *testing.T) {
			r := mergeRig(t, time.Hour, SideInfo{ID: "host", Location: "host"})
			// The pod is across a network: a delivery takes longer than
			// the file goes between changes.
			pod := r.faulty(SideInfo{ID: "pod", Location: "a"})
			pod.set(func(f *faultyPeer) { f.delay = 40 * time.Millisecond })
			r.write("host", "files", ".claude.json", `{"numStartups":1}`)
			r.sync()
			r.write("host", "files", ".claude.json", `{"numStartups":2}`)
			r.scan("host")

			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				for n := 3; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					r.write(churner, "files", ".claude.json", fmt.Sprintf(`{"numStartups":%d,"%s":%d}`, n, churner, n))
					_ = r.sides[churner].side.Rescan(context.Background())
					time.Sleep(10 * time.Millisecond)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := r.hub.FlushSide(ctx, "pod", func(root, p string) bool { return p == ".claude.json" })
			cancel()
			close(stop)
			<-done
			if err != nil {
				t.Fatalf("FlushSide chased the churn: %v", err)
			}
		})
	}
}

// A side slower than a settings file's changes still gets them: the hub
// sends the state a batch names from its own copy when the path has moved
// on, rather than refusing it as stale until the changes stop.
func TestSlowSideGetsAFileThatKeepsChanging(t *testing.T) {
	r := mergeRig(t, 0, SideInfo{ID: "host", Location: "host"})
	pod := r.faulty(SideInfo{ID: "pod", Location: "a"})
	pod.set(func(f *faultyPeer) { f.delay = 40 * time.Millisecond })
	r.write("host", "files", ".claude.json", `{"numStartups":1}`)
	r.sync()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 2; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			r.write("host", "files", ".claude.json", fmt.Sprintf(`{"numStartups":%d}`, n))
			_ = r.sides["host"].side.Rescan(context.Background())
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for r.read("pod", "files", ".claude.json") == `{"numStartups":1}` {
		if time.Now().After(deadline) {
			t.Fatalf("the pod got none of the changes:\n%s", r.dump())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A conflict copy the daemon host has no room for holds its decision back,
// and Status says so.
func TestStatusSaysTheDaemonHostIsTooFullForAConflictCopy(t *testing.T) {
	free := int64(1 << 30)
	r := &rig{t: t, base: t.TempDir(), table: testTable(), roots: testRoots, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.hopts = HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond, Logger: r.logs.logger(),
		Space: func(string) (Space, error) { return Space{Total: 1 << 30, Free: free}, nil }}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1} }
	r.openHub()
	t.Cleanup(r.close)
	r.addSide(SideInfo{ID: "a", Location: "a"})
	r.addSide(SideInfo{ID: "b", Location: "b"})
	if got := r.hub.Status().PauseReasons(); len(got) != 0 {
		t.Fatalf("pause reasons before: %v", got)
	}
	r.write("a", "user", "f.txt", "a")
	r.sync()
	free = 1 << 20 // under 5% of 1GiB
	r.write("a", "user", "f.txt", "a2")
	r.write("b", "user", "f.txt", "b2")
	_ = r.flushErr(500 * time.Millisecond)
	want := "the daemon host's disk is under 5% free"
	if got := r.hub.Status().HostPaused; got != want {
		t.Errorf("Status.HostPaused %q, want %q", got, want)
	}
	if got := r.hub.Status().PauseReasons(); len(got) == 0 || got[0] != want {
		t.Errorf("PauseReasons %v", got)
	}
	free = 1 << 30
	r.sync()
	if got := r.hub.Status().HostPaused; got != "" {
		t.Errorf("Status.HostPaused once there is room: %q", got)
	}
}
