//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// execClass maps the backend's two exec classes to execas's.
func TestExecClassMapping(t *testing.T) {
	if execClass(backend.ExecClassExec) != execas.ClassExec {
		t.Error("exec class")
	}
	if execClass(backend.ExecClassScript) != execas.ClassScript {
		t.Error("script class")
	}
}

// fallbackable: a command may fall back to a plain kube exec after the agent
// bootstrap failed for a transient reason (timeout), but not when the pod
// failed verification, is not up, the connection is gone, or the caller gave
// up.
func TestFallbackable(t *testing.T) {
	tampered := &TamperedError{Namespace: "ns", Pod: "p"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", &DeadlineError{Kind: CodeTimeout}, true},
		{"generic", errors.New("some transient failure"), true},
		{"tampered", tampered, false},
		{"unverifiable", ErrUnverifiable, false},
		{"not-ready", ErrPodNotReady, false},
		{"no-cluster", ErrClusterNotFound, false},
		{"unbound", errUnbound, false},
		{"no-record", errNoInstanceRecord, false},
		{"cancelled", context.Canceled, false},
		{"deadline-ctx", context.DeadlineExceeded, false},
	} {
		if got := fallbackable(tc.err); got != tc.want {
			t.Errorf("%s: fallbackable(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
