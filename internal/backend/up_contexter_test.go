package backend

import (
	"context"
	"errors"
	"testing"
)

// upOnly is a Backend with only the plain Up; any other method panics on the
// nil embedded interface, so a test fails loudly if UpContext reaches for one.
type upOnly struct {
	Backend
	gotDir    string
	gotMounts []Mount
}

func (u *upOnly) Up(workspaceDir string, mounts []Mount) (*UpResult, error) {
	u.gotDir, u.gotMounts = workspaceDir, mounts
	return &UpResult{ContainerID: "plain"}, nil
}

// upWithContext also has UpContext; its plain Up must never be called.
type upWithContext struct {
	upOnly
	gotCtx context.Context
	gotReq UpRequest
}

func (u *upWithContext) Up(string, []Mount) (*UpResult, error) {
	return nil, errors.New("plain Up called on an UpContexter")
}

func (u *upWithContext) UpContext(ctx context.Context, req UpRequest) (*UpResult, error) {
	u.gotCtx, u.gotReq = ctx, req
	return &UpResult{ContainerID: "ctx"}, nil
}

type ctxMarker struct{}

func TestUpContextPrefersUpContexterAndFallsBackToUp(t *testing.T) {
	req := UpRequest{WorkspaceDir: "/ws", Mounts: []Mount{{LocalPath: "/h", ContainerPath: "/c"}}}
	ctx := context.WithValue(context.Background(), ctxMarker{}, "job")

	withCtx := &upWithContext{}
	res, err := UpContext(ctx, withCtx, req)
	if err != nil || res.ContainerID != "ctx" {
		t.Fatalf("UpContexter: got %+v, %v; want its UpContext result", res, err)
	}
	if withCtx.gotCtx.Value(ctxMarker{}) != "job" {
		t.Fatal("UpContext did not receive the caller's ctx")
	}
	if withCtx.gotReq.WorkspaceDir != "/ws" || len(withCtx.gotReq.Mounts) != 1 {
		t.Fatalf("UpContext got request %+v, want %+v", withCtx.gotReq, req)
	}

	plain := &upOnly{}
	res, err = UpContext(ctx, plain, req)
	if err != nil || res.ContainerID != "plain" {
		t.Fatalf("plain backend: got %+v, %v; want its Up result", res, err)
	}
	if plain.gotDir != "/ws" || len(plain.gotMounts) != 1 || plain.gotMounts[0].ContainerPath != "/c" {
		t.Fatalf("Up got (%q, %+v), want the request's workspace dir and mounts", plain.gotDir, plain.gotMounts)
	}
}
