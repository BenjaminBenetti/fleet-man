package kubeimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kube", "dev-abc123", "images.json")
	var f indexFiles
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	supported := false
	err := f.update(path, func(ix *Index) error {
		e := ix.put(Entry{ImageID: imageID("a"), Strategy: StrategyRegistry, Location: "r.io/fleet/3fa2c91e/devcontainer", Ref: "r.io/x@" + zeroDigest, DeliveredAt: t0})
		e.hold("inst-1")
		ix.Registries = map[string]RegistryState{"r.io": {DeleteSupported: &supported, Reason: "no", CheckedAt: t0}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := f.read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Images) != 1 || ix.Images[0].Holders[0] != "inst-1" || ix.Version != indexVersion || *ix.Registries["r.io"].DeleteSupported {
		t.Fatalf("index %+v", ix)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		di, _ := os.Stat(filepath.Dir(path))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("modes %v %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}

	// put keeps the holders the index has.
	_ = f.update(path, func(ix *Index) error {
		ix.put(Entry{ImageID: imageID("a"), Strategy: StrategyRegistry, Location: "r.io/fleet/3fa2c91e/devcontainer", Ref: "new"})
		return nil
	})
	ix, _ = f.read(path)
	if ix.Images[0].Ref != "new" || len(ix.Images[0].Holders) != 1 {
		t.Errorf("after put %+v", ix.Images[0])
	}

	// A no-op update does not rewrite the file.
	before, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	_ = f.update(path, func(*Index) error { return nil })
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a no-op update rewrote the index")
	}
	// An error from fn leaves the index unsaved.
	_ = f.update(path, func(ix *Index) error { ix.Images = nil; return errors.New("no") })
	if ix, _ := f.read(path); len(ix.Images) != 1 {
		t.Error("a failed update was saved")
	}
}

func TestIndexCorruptFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "images.json")
	for _, bad := range []string{"{not json", `{"version":99,"images":[]}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		ix, err := loadIndex(path)
		if err != nil || len(ix.Images) != 0 {
			t.Fatalf("%q: %+v, %v", bad, ix, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%q: the corrupt index is still in place", bad)
		}
		matches, _ := filepath.Glob(path + ".corrupt-*")
		if len(matches) == 0 {
			t.Errorf("%q: nothing moved aside", bad)
		}
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

func TestEntryHoldRelease(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	e := Entry{DeliveredAt: t0}
	if !e.unheldSince().Equal(t0) {
		t.Error("an entry never held is unheld since delivery")
	}
	e.hold("b")
	e.hold("a")
	e.hold("a")
	if strings.Join(e.Holders, ",") != "a,b" {
		t.Errorf("holders %v", e.Holders)
	}
	if e.release("x", t0.Add(time.Hour)) {
		t.Error("released a holder it did not have")
	}
	e.release("a", t0.Add(time.Hour))
	if !e.ReleasedAt.IsZero() {
		t.Error("released while still held")
	}
	e.release("b", t0.Add(2*time.Hour))
	if e.Holders != nil || !e.unheldSince().Equal(t0.Add(2*time.Hour)) {
		t.Errorf("after the last release %+v", e)
	}
	e.hold("c")
	if !e.ReleasedAt.IsZero() {
		t.Error("holding again keeps the release time")
	}
}

func TestFlightSharesOneRun(t *testing.T) {
	var f flight[int]
	var runs atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := f.do(context.Background(), "k", func(context.Context) (int, error) {
				runs.Add(1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}(i)
	}
	// Let every caller join the flight before it lands.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Errorf("%d runs", runs.Load())
	}
	for _, v := range results {
		if v != 42 {
			t.Errorf("results %v", results)
		}
	}
	// Another key, and the same key afterwards, run again.
	_, _ = f.do(context.Background(), "k", func(context.Context) (int, error) { runs.Add(1); return 0, nil })
	if runs.Load() != 2 {
		t.Errorf("%d runs", runs.Load())
	}
}

// TestFlightWaiterRunsAgainWhenTheLeaderIsCancelled: a leader that stopped
// for its own context's sake hands its waiters nothing; a waiter still live
// runs the work itself.
func TestFlightWaiterRunsAgainWhenTheLeaderIsCancelled(t *testing.T) {
	var f flight[string]
	leaderCtx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	leaderDone := make(chan error)
	go func() {
		_, err := f.do(leaderCtx, "k", func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
		leaderDone <- err
	}()
	<-started
	waiterDone := make(chan string)
	go func() {
		v, err := f.do(context.Background(), "k", func(context.Context) (string, error) { return "mine", nil })
		if err != nil {
			t.Error(err)
		}
		waiterDone <- v
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader %v", err)
	}
	if v := <-waiterDone; v != "mine" {
		t.Errorf("waiter got %q", v)
	}

	// A waiter whose own context ends stops waiting.
	block := make(chan struct{})
	go func() {
		_, _ = f.do(context.Background(), "x", func(context.Context) (string, error) { <-block; return "", nil })
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := f.do(ctx, "x", func(context.Context) (string, error) { return "", nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter %v", err)
	}
	close(block)
}

func TestKeyLocks(t *testing.T) {
	var k keyLocks
	unlock := k.lock("a")
	got := make(chan bool)
	go func() {
		u := k.lock("a")
		// Unlocked before it says so: the count below must see it gone.
		u()
		got <- true
	}()
	select {
	case <-got:
		t.Fatal("a second lock of a held key went through")
	case <-time.After(30 * time.Millisecond):
	}
	// Another key is not blocked.
	k.lock("b")()
	unlock()
	<-got
	k.mu.Lock()
	n := len(k.m)
	k.mu.Unlock()
	if n != 0 {
		t.Errorf("%d locks left", n)
	}
}
