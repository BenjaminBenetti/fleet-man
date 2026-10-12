package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
)

// writeDevcontainer writes devcontainer.json into a checkout dir.
func writeDevcontainer(t *testing.T, dir, json string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The Deploy row reads a fleet's devcontainer.json from its newest checkout
// on the daemon host, else from the last inspection of its remote.
func TestFleetRepoConfig(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	old, newer := t.TempDir(), t.TempDir()
	writeDevcontainer(t, old, `{"dockerComposeFile": "c.yml"}`)
	writeDevcontainer(t, newer, `{"image": "alpine:3", "privileged": true}`)
	now := time.Now()
	f := &fleet.Fleet{Name: "a", Remote: "git@example.com:a.git", Instances: []*fleet.Instance{
		{Name: "old", WorkspaceDir: old, CreatedAt: now.Add(-time.Hour)},
		{Name: "new", WorkspaceDir: newer, CreatedAt: now},
	}}
	if got, want := svc.fleetRepoConfig(f), (kubecheck.Config{Privileged: "--privileged", Prebuilt: "alpine:3"}); got != want {
		t.Errorf("newest checkout: %+v, want %+v", got, want)
	}
	// A checkout without a configuration is skipped for the next one.
	if err := os.RemoveAll(filepath.Join(newer, ".devcontainer")); err != nil {
		t.Fatal(err)
	}
	if got := svc.fleetRepoConfig(f); !got.Compose {
		t.Errorf("next checkout: %+v, want compose", got)
	}
	// No checkout: what InspectRepo read of the remote.
	bare := &fleet.Fleet{Name: "b", Remote: "git@example.com:b.git"}
	if got := svc.fleetRepoConfig(bare); got != (kubecheck.Config{}) {
		t.Errorf("never inspected: %+v", got)
	}
	svc.repoChecks.put(bare.Remote, kubecheck.Config{Compose: true})
	if got := svc.fleetRepoConfig(bare); !got.Compose {
		t.Errorf("inspected: %+v, want compose", got)
	}
	if got := svc.fleetRepoConfig(nil); got != (kubecheck.Config{}) {
		t.Errorf("no fleet: %+v", got)
	}
}

// A template fleet's own folder is its checkout.
func TestFleetRepoConfigOfATemplate(t *testing.T) {
	isolateFleetDir(t)
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{"dockerComposeFile": ["a.yml"]}`)
	f := &fleet.Fleet{Name: "t", Remote: "file://" + dir}
	if got := newService().fleetRepoConfig(f); !got.Compose {
		t.Errorf("template: %+v, want compose", got)
	}
}

// InspectRepo keeps what its clone of the default branch asks of a cluster,
// for the fleet it is about to add; an inspection of another branch is not
// what instances get by default, and is not kept.
func TestInspectRepoKeepsTheKubernetesCheck(t *testing.T) {
	isolateFleetDir(t)
	stubInspectorOpen(t, func(remoteURL, branch string) (*inspector.Repo, error) {
		return &inspector.Repo{Root: fakeRepoDir(t, `{"dockerComposeFile": "c.yml", "service": "app"}`)}, nil
	})
	svc := newService()
	if _, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "git@example.com:other.git", Branch: "dev"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.repoChecks.get("git@example.com:other.git"); ok {
		t.Error("an inspection of another branch was kept")
	}
	if _, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "git@example.com:c.git"}); err != nil {
		t.Fatal(err)
	}
	if cfg, ok := svc.repoChecks.get("git@example.com:c.git"); !ok || !cfg.Compose {
		t.Errorf("kept %+v, %v; want compose", cfg, ok)
	}
}

func TestRepoChecksAreBounded(t *testing.T) {
	var r repoChecks
	for i := 0; i < repoCheckLimit; i++ {
		r.put(fmt.Sprintf("git@example.com:r%d.git", i), kubecheck.Config{})
	}
	r.put("last", kubecheck.Config{Compose: true})
	if len(r.byRemote) != 1 {
		t.Errorf("%d inspections kept past the limit, want 1", len(r.byRemote))
	}
	if cfg, ok := r.get("last"); !ok || !cfg.Compose {
		t.Errorf("the newest inspection: %+v, %v", cfg, ok)
	}
}
