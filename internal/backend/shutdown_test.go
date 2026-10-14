package backend

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestShutdownCut(t *testing.T) {
	live := context.Background()
	shut, cancelShut := context.WithCancelCause(live)
	cancelShut(fmt.Errorf("stopping: %w", ErrDaemonShutdown))
	destroyed, cancelDestroyed := context.WithCancelCause(live)
	cancelDestroyed(errors.New("instance destroyed"))
	plain, cancelPlain := context.WithCancel(live)
	cancelPlain()
	child, cancelChild := context.WithCancel(shut)
	defer cancelChild()
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"running":         {live, false},
		"daemon shutdown": {shut, true},
		"its child":       {child, true},
		"a destroy":       {destroyed, false},
		"a plain cancel":  {plain, false},
	} {
		if got := ShutdownCut(tc.ctx); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
