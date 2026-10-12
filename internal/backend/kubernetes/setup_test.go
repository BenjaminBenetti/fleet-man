//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// TestSetUpErrorNamesTheDeadline: a set-up its own deadline stopped fails
// kubernetes:timeout, naming what still ran in the pod and the override that
// sets the deadline; one its caller cancelled, or that failed of its own,
// keeps its error.
func TestSetUpErrorNamesTheDeadline(t *testing.T) {
	stuck := &devcli.TimeoutError{Running: []string{`"/bin/bash -lic echo -n m" as vscode for 59m58s`}, Err: errors.New("devcontainer set-up (lifecycle hooks) failed: context deadline exceeded")}
	err := setUpError(context.Background(), time.Hour, stuck)
	var de *DeadlineError
	if !errors.As(err, &de) || de.Code() != CodeTimeout {
		t.Fatalf("a set-up past its deadline: %v, want a kubernetes:timeout DeadlineError", err)
	}
	want := `kubernetes:timeout: gave up waiting for devcontainer set-up (the lifecycle hooks) after 1h0m0s: still running in the pod: "/bin/bash -lic echo -n m" as vscode for 59m58s (FLEET_KUBE_SETUP_TIMEOUT sets this deadline)`
	if err.Error() != want {
		t.Errorf("message\n%s\nwant\n%s", err, want)
	}
	if code, ok := kubeerr.Of(classify(err)); !ok || code != kubeerr.Code(CodeTimeout) {
		t.Errorf("classified %v %v, want %s", code, ok, CodeTimeout)
	}
	if !errors.Is(err, stuck) {
		t.Error("the deadline error does not carry the set-up's own")
	}

	noExec := &devcli.TimeoutError{Err: errors.New("killed")}
	if err := setUpError(context.Background(), time.Minute, noExec); !strings.Contains(err.Error(), "the devcontainer CLI was still running") {
		t.Errorf("a set-up with nothing running in the pod: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := setUpError(cancelled, time.Hour, stuck); err != stuck {
		t.Errorf("a set-up its caller ended: %v, want its own error", err)
	}
	plain := errors.New("postCreateCommand from devcontainer.json failed")
	if err := setUpError(context.Background(), time.Hour, plain); err != plain {
		t.Errorf("a failed hook: %v, want its own error", err)
	}
	if err := setUpError(context.Background(), time.Hour, nil); err != nil {
		t.Errorf("a set-up that succeeded: %v", err)
	}
}
