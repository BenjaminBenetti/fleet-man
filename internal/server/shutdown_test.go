package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The steps run in order within one budget: each gets its own deadline,
// capped by the end of the budget; one that does not heed its context is
// left behind a moment past its deadline; one the budget left no time for
// still runs, with its context already done.
func TestRunShutdownSteps(t *testing.T) {
	orig := stepGrace
	stepGrace = 50 * time.Millisecond
	t.Cleanup(func() { stepGrace = orig })

	var mu sync.Mutex
	var order []string
	left := map[string]time.Duration{}
	enter := func(name string, ctx context.Context) {
		dl, _ := ctx.Deadline()
		mu.Lock()
		order = append(order, name)
		left[name] = time.Until(dl)
		mu.Unlock()
	}
	var deafDone atomic.Bool
	steps := []shutdownStep{
		{name: "quick", limit: 100 * time.Millisecond, run: func(ctx context.Context) { enter("quick", ctx) }},
		{name: "deaf", limit: 100 * time.Millisecond, run: func(ctx context.Context) {
			enter("deaf", ctx)
			time.Sleep(2 * time.Second)
			deafDone.Store(true)
		}},
		{name: "capped", limit: time.Hour, run: func(ctx context.Context) {
			enter("capped", ctx)
			<-ctx.Done()
		}},
		{name: "late", run: func(ctx context.Context) {
			enter("late", ctx)
			if ctx.Err() == nil {
				t.Error("the last step had time left after the budget")
			}
		}},
	}
	start := time.Now()
	runShutdownSteps(600*time.Millisecond, steps)
	took := time.Since(start)

	if took < 600*time.Millisecond || took > 1500*time.Millisecond {
		t.Fatalf("the steps took %v, want about the 600ms budget", took)
	}
	if deafDone.Load() {
		t.Fatal("the steps waited for a step past its deadline")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"quick", "deaf", "capped", "late"}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] || order[3] != want[3] {
		t.Fatalf("order %v, want %v", order, want)
	}
	if left["quick"] > 100*time.Millisecond || left["deaf"] > 100*time.Millisecond {
		t.Errorf("own deadlines: %v", left)
	}
	// The capped step started after quick (~0) and deaf (~150ms): the end
	// of the budget, not its hour.
	if l := left["capped"]; l > 500*time.Millisecond || l < 200*time.Millisecond {
		t.Errorf("capped step had %v, want the rest of the budget", l)
	}
	if left["late"] > 0 {
		t.Errorf("the late step had %v left", left["late"])
	}
}
