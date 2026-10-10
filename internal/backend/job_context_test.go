package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestJobContextCarriesHooksAndJobLog(t *testing.T) {
	bare := context.Background()
	if h := HooksFrom(bare); h.EnsureAgent != nil || h.SharedStateReady != nil {
		t.Fatalf("HooksFrom(bare ctx) = %+v, want the zero Hooks", h)
	}
	if w := JobLogFrom(bare); w != io.Discard {
		t.Fatalf("JobLogFrom(bare ctx) = %T, want io.Discard", w)
	}
	if w := JobLogFrom(WithJobLog(bare, nil)); w != io.Discard {
		t.Fatalf("JobLogFrom(nil writer) = %T, want io.Discard", w)
	}

	var calls []string
	var log bytes.Buffer
	ctx := WithJobLog(WithHooks(bare, Hooks{
		EnsureAgent: func(context.Context) error { calls = append(calls, "agent"); return nil },
		SharedStateReady: func(context.Context) error {
			calls = append(calls, "shared")
			return errors.New("not synced")
		},
	}), &log)

	h := HooksFrom(ctx)
	if err := h.EnsureAgent(ctx); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if err := h.SharedStateReady(ctx); err == nil || err.Error() != "not synced" {
		t.Fatalf("SharedStateReady error = %v, want the hook's own error", err)
	}
	if len(calls) != 2 || calls[0] != "agent" || calls[1] != "shared" {
		t.Fatalf("hook calls = %v, want [agent shared]", calls)
	}

	if _, err := io.WriteString(JobLogFrom(ctx), "step 1\n"); err != nil {
		t.Fatal(err)
	}
	if log.String() != "step 1\n" {
		t.Fatalf("job log = %q, want the write to reach the attached writer", log.String())
	}
}
