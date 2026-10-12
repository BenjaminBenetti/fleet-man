package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

func TestLoadDeadlinesDefaults(t *testing.T) {
	d, bad := LoadDeadlines(func(string) string { return "" })
	want := Deadlines{
		PVCBind: 5 * time.Minute, Unschedulable: 3 * time.Minute, ImagePull: 15 * time.Minute,
		PodReady: 10 * time.Minute, Agent: 2 * time.Minute, SharedState: 60 * time.Second,
		Snapshot: 30 * time.Minute, Copy: 2 * time.Hour, SetUp: time.Hour,
	}
	if d != want || len(bad) != 0 {
		t.Fatalf("defaults = %+v %v, want %+v", d, bad, want)
	}
	if got := d.SeedDeadline(3 << 30); got != jobboot.SeedDeadline(3<<30) {
		t.Errorf("seed deadline %v, want jobboot's %v", got, jobboot.SeedDeadline(3<<30))
	}
}

func TestLoadDeadlinesOverrides(t *testing.T) {
	env := map[string]string{
		EnvPVCBindTimeout:       "90s",
		EnvUnschedulableTimeout: "120",
		EnvImagePullTimeout:     " 1h ",
		EnvSeedTimeout:          "45m",
		EnvPodReadyTimeout:      "0",
		EnvAgentTimeout:         "-5s",
		EnvSharedStateTimeout:   "soon",
		EnvSnapshotTimeout:      "3m",
		EnvCopyTimeout:          "600",
		EnvSetUpTimeout:         "20m",
	}
	d, bad := LoadDeadlines(func(k string) string { return env[k] })
	if d.PVCBind != 90*time.Second || d.Unschedulable != 2*time.Minute || d.ImagePull != time.Hour || d.Seed != 45*time.Minute || d.Snapshot != 3*time.Minute || d.Copy != 10*time.Minute || d.SetUp != 20*time.Minute {
		t.Errorf("overrides not taken: %+v", d)
	}
	if d.PodReady != DefaultPodReadyTimeout || d.Agent != DefaultAgentTimeout || d.SharedState != DefaultSharedStateTimeout {
		t.Errorf("malformed overrides changed a deadline: %+v", d)
	}
	if got := strings.Join(bad, ","); got != EnvPodReadyTimeout+"=0,"+EnvAgentTimeout+"=-5s,"+EnvSharedStateTimeout+"=soon" {
		t.Errorf("bad overrides = %q", got)
	}
	if got := d.SeedDeadline(100 << 30); got != 45*time.Minute {
		t.Errorf("a seed override is a fixed deadline: %v", got)
	}
}

func TestDeadlineError(t *testing.T) {
	cause := context.DeadlineExceeded
	err := error(&DeadlineError{Kind: CodePVCPending, Wait: "the instance pod's disk (PVC x) to bind", After: 5 * time.Minute, Detail: "still Pending", Env: EnvPVCBindTimeout, Err: cause})
	want := "kubernetes:pvc_pending: gave up waiting for the instance pod's disk (PVC x) to bind after 5m0s: still Pending (FLEET_KUBE_PVC_BIND_TIMEOUT sets this deadline)"
	if err.Error() != want {
		t.Errorf("message\n%s\nwant\n%s", err.Error(), want)
	}
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != CodePVCPending {
		t.Errorf("code = %v", coded)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the wait's own error is not unwrapped")
	}
	if CodeImagePull != "kubernetes:image_pull" {
		t.Errorf("image pull code %q", CodeImagePull)
	}
}
