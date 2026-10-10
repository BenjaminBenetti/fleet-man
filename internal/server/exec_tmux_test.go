package server

import (
	"context"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// blockingTmux is a backend whose tmux check never finishes (its pod's agent
// is reconnecting) until released.
type blockingTmux struct {
	backend.Backend
	release chan struct{}
	ran     chan struct{}
}

func (b *blockingTmux) EnsureTmux(ctx context.Context) error {
	defer close(b.ran)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

// An attach waits for the tmux check at most ensureTmuxWait, and the check
// goes on after it (a slow agent never fails the attach's resolve).
func TestEnsureInstanceTmuxIsBounded(t *testing.T) {
	old := ensureTmuxWait
	ensureTmuxWait = 50 * time.Millisecond
	t.Cleanup(func() { ensureTmuxWait = old })
	b := &blockingTmux{release: make(chan struct{}), ran: make(chan struct{})}
	start := time.Now()
	ensureInstanceTmux(b)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the attach waited %s for the tmux check", waited)
	}
	select {
	case <-b.ran:
		t.Fatal("the check was cut short when the attach stopped waiting")
	default:
	}
	close(b.release)
	select {
	case <-b.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the background check never finished")
	}
}
