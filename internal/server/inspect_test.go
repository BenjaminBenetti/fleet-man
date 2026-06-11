package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubInspectorOpen swaps the inspector.Open seam for the duration of the
// test, restoring it on cleanup.
func stubInspectorOpen(t *testing.T, fn func(remoteURL, branch string) (*inspector.Repo, error)) {
	t.Helper()
	orig := inspectorOpen
	inspectorOpen = fn
	t.Cleanup(func() { inspectorOpen = orig })
}

// fakeRepoDir builds a throwaway "clone" directory. When devcontainerJSON is
// non-empty it is written to .devcontainer/devcontainer.json. Each call makes
// a fresh dir because the handler's repo.Close() removes it.
func fakeRepoDir(t *testing.T, devcontainerJSON string) string {
	t.Helper()
	root := t.TempDir()
	if devcontainerJSON != "" {
		dir := filepath.Join(root, ".devcontainer")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "devcontainer.json"), []byte(devcontainerJSON), 0o644); err != nil {
			t.Fatalf("write devcontainer.json: %v", err)
		}
	}
	return root
}

func TestInspectRepoEmptyURLIsInvalidArgument(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	_, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestInspectRepoCloneFailureIsFailedPrecondition(t *testing.T) {
	isolateFleetDir(t)
	stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
		return nil, errors.New("clone: git@github.com: Permission denied (publickey)")
	})
	svc := newService()

	_, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "git@github.com:x/y.git"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	// The clone error text is shown to the user verbatim — it must survive.
	if !strings.Contains(status.Convert(err).Message(), "Permission denied (publickey)") {
		t.Fatalf("clone error text lost: %v", err)
	}
}

func TestInspectRepoReportsDevcontainerPresence(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()

	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{name: "present", json: "{}", want: true},
		{name: "absent", json: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRemote, gotBranch string
			stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
				gotRemote, gotBranch = remoteURL, branch
				return &inspector.Repo{Root: fakeRepoDir(t, tc.json)}, nil
			})

			reply, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{
				RemoteUrl: "git@example.com:a/b.git", Branch: "dev",
			})
			if err != nil {
				t.Fatalf("InspectRepo: %v", err)
			}
			if reply.GetHasDevcontainer() != tc.want {
				t.Fatalf("HasDevcontainer = %v, want %v", reply.GetHasDevcontainer(), tc.want)
			}
			if reply.GetHomeDir() != "" {
				t.Fatalf("HomeDir should be empty when detect_home_dir unset, got %q", reply.GetHomeDir())
			}
			if gotRemote != "git@example.com:a/b.git" || gotBranch != "dev" {
				t.Fatalf("Open called with (%q, %q)", gotRemote, gotBranch)
			}
		})
	}
}

func TestInspectRepoDetectsHomeDirFromRemoteUser(t *testing.T) {
	isolateFleetDir(t)
	stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
		return &inspector.Repo{Root: fakeRepoDir(t, `{"remoteUser": "vscode"}`)}, nil
	})
	svc := newService()

	reply, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{
		RemoteUrl: "git@example.com:a/b.git", DetectHomeDir: true,
	})
	if err != nil {
		t.Fatalf("InspectRepo: %v", err)
	}
	if !reply.GetHasDevcontainer() {
		t.Fatalf("want HasDevcontainer true")
	}
	if reply.GetHomeDir() != "/home/vscode" {
		t.Fatalf("HomeDir = %q, want /home/vscode", reply.GetHomeDir())
	}
}

func TestInspectRepoHomeDirNoHintIsEmptyNotError(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()

	// No devcontainer.json at all: detection hits ErrNoDevcontainerConfig.
	// A devcontainer.json without user/image fields: ErrNoUserHint.
	for _, json := range []string{"", "{}"} {
		stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
			return &inspector.Repo{Root: fakeRepoDir(t, json)}, nil
		})
		reply, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{
			RemoteUrl: "git@example.com:a/b.git", DetectHomeDir: true,
		})
		if err != nil {
			t.Fatalf("InspectRepo(json=%q): %v", json, err)
		}
		if reply.GetHomeDir() != "" {
			t.Fatalf("HomeDir = %q, want empty (no hint)", reply.GetHomeDir())
		}
	}
}

func TestInspectRepoClosesRepo(t *testing.T) {
	isolateFleetDir(t)
	root := fakeRepoDir(t, "{}")
	stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
		return &inspector.Repo{Root: root}, nil
	})
	svc := newService()

	if _, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "u"}); err != nil {
		t.Fatalf("InspectRepo: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temp clone not removed: stat err = %v", err)
	}
}
