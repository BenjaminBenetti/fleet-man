package backend

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// recordingReporter is a provisioning-log sink that records steps and warnings.
type recordingReporter struct {
	bytes.Buffer
	steps    []fleet.JobStep
	details  []string
	warnings []string
}

func (r *recordingReporter) Step(step fleet.JobStep, detail string) {
	r.steps = append(r.steps, step)
	r.details = append(r.details, detail)
}

func (r *recordingReporter) Warn(message string) { r.warnings = append(r.warnings, message) }

func TestReportStepAndWarningReachAJobReporter(t *testing.T) {
	rec := &recordingReporter{}
	ctx := WithJobLog(context.Background(), rec)

	ReportStep(ctx, fleet.StepGitClone, "git@x:a.git")
	ReportWarning(ctx, "dotfiles failed\nsecond line")

	if len(rec.steps) != 1 || rec.steps[0] != fleet.StepGitClone || rec.details[0] != "git@x:a.git" {
		t.Fatalf("steps = %v %q, want the clone step with its detail", rec.steps, rec.details)
	}
	if len(rec.warnings) != 1 || rec.warnings[0] != "dotfiles failed\nsecond line" {
		t.Fatalf("warnings = %q, want the whole message once", rec.warnings)
	}
	if rec.Len() != 0 {
		t.Fatalf("a JobReporter got text written as well: %q", rec.String())
	}
}

func TestReportStepAndWarningWritePlainSinksLines(t *testing.T) {
	var log bytes.Buffer
	ctx := WithJobLog(context.Background(), &log)

	ReportStep(ctx, fleet.StepBackendUp, "devcontainer up")
	ReportStep(ctx, fleet.StepDone, "")
	ReportWarning(ctx, "a\nb\n")

	want := "==> starting container: devcontainer up\n==> done\nwarning: a\nwarning: b\n"
	if log.String() != want {
		t.Fatalf("plain sink got %q, want %q", log.String(), want)
	}

	// No sink attached: both are no-ops (and must not panic).
	ReportStep(context.Background(), fleet.StepDone, "")
	ReportWarning(context.Background(), "ignored")
}

func TestStepLine(t *testing.T) {
	for _, tt := range []struct {
		step   fleet.JobStep
		detail string
		want   string
	}{
		{fleet.StepGitClone, "", "cloning repository"},
		{fleet.StepGitClone, "--branch dev", "cloning repository: --branch dev"},
		{fleet.JobStep(4242), "future step", "future step"},
	} {
		if got := StepLine(tt.step, tt.detail); got != tt.want {
			t.Errorf("StepLine(%d, %q) = %q, want %q", tt.step, tt.detail, got, tt.want)
		}
	}
}

// rebuildOnly is a Backend with only the plain Rebuild.
type rebuildOnly struct {
	Backend
	gotID, gotDir string
}

func (r *rebuildOnly) Rebuild(containerID, workspaceDir string, _ []Mount) (*UpResult, error) {
	r.gotID, r.gotDir = containerID, workspaceDir
	return &UpResult{ContainerID: "plain"}, nil
}

// rebuildWithContext also has RebuildContext; its plain Rebuild must never run.
type rebuildWithContext struct {
	rebuildOnly
	gotCtx context.Context
	gotReq UpRequest
}

func (r *rebuildWithContext) Rebuild(string, string, []Mount) (*UpResult, error) {
	return nil, errors.New("plain Rebuild called on a RebuildContexter")
}

func (r *rebuildWithContext) RebuildContext(ctx context.Context, containerID string, req UpRequest) (*UpResult, error) {
	r.gotCtx, r.gotID, r.gotReq = ctx, containerID, req
	return &UpResult{ContainerID: "ctx"}, nil
}

func TestRebuildContextPrefersRebuildContexterAndFallsBack(t *testing.T) {
	req := UpRequest{WorkspaceDir: "/ws"}
	ctx := context.WithValue(context.Background(), ctxMarker{}, "job")

	withCtx := &rebuildWithContext{}
	res, err := RebuildContext(ctx, withCtx, "old", req)
	if err != nil || res.ContainerID != "ctx" {
		t.Fatalf("RebuildContexter: got %+v, %v; want its RebuildContext result", res, err)
	}
	if withCtx.gotCtx.Value(ctxMarker{}) != "job" || withCtx.gotID != "old" || withCtx.gotReq.WorkspaceDir != "/ws" {
		t.Fatalf("RebuildContext got ctx=%v id=%q req=%+v", withCtx.gotCtx, withCtx.gotID, withCtx.gotReq)
	}

	plain := &rebuildOnly{}
	res, err = RebuildContext(ctx, plain, "old", req)
	if err != nil || res.ContainerID != "plain" || plain.gotID != "old" || plain.gotDir != "/ws" {
		t.Fatalf("plain backend: got %+v, %v (id %q dir %q); want its Rebuild result", res, err, plain.gotID, plain.gotDir)
	}
}

// cloneOnly has only the plain Clone.
type cloneOnly struct {
	Backend
	gotSrc, gotDir string
}

func (c *cloneOnly) Clone(src, destWorkspaceDir string, _ []Mount) (*UpResult, error) {
	c.gotSrc, c.gotDir = src, destWorkspaceDir
	return &UpResult{ContainerID: "plain"}, nil
}

// cloneWithContext also has CloneContext; its plain Clone must never run.
type cloneWithContext struct {
	cloneOnly
	gotCtx context.Context
	gotReq UpRequest
}

func (c *cloneWithContext) Clone(string, string, []Mount) (*UpResult, error) {
	return nil, errors.New("plain Clone called on a CloneContexter")
}

func (c *cloneWithContext) CloneContext(ctx context.Context, src string, req UpRequest) (*UpResult, error) {
	c.gotCtx, c.gotSrc, c.gotReq = ctx, src, req
	return &UpResult{ContainerID: "ctx"}, nil
}

func TestCloneContextPrefersCloneContexterAndFallsBack(t *testing.T) {
	req := UpRequest{WorkspaceDir: "/dest"}
	ctx := context.WithValue(context.Background(), ctxMarker{}, "job")

	withCtx := &cloneWithContext{}
	res, err := CloneContext(ctx, withCtx, "src", req)
	if err != nil || res.ContainerID != "ctx" {
		t.Fatalf("CloneContexter: got %+v, %v", res, err)
	}
	if withCtx.gotCtx.Value(ctxMarker{}) != "job" || withCtx.gotSrc != "src" || withCtx.gotReq.WorkspaceDir != "/dest" {
		t.Fatalf("CloneContext got ctx=%v src=%q req=%+v", withCtx.gotCtx, withCtx.gotSrc, withCtx.gotReq)
	}

	plain := &cloneOnly{}
	res, err = CloneContext(ctx, plain, "src", req)
	if err != nil || res.ContainerID != "plain" || plain.gotSrc != "src" || plain.gotDir != "/dest" {
		t.Fatalf("plain backend: got %+v, %v (src %q dir %q)", res, err, plain.gotSrc, plain.gotDir)
	}
}
