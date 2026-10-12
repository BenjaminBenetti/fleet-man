//go:build !nok8s

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// A removed connection's release runs in the background; waitCleanups waits
// for it, so its writes (the connection's directory going) land while the
// HOME it started under is still there, and never after a test handed it
// back (they used to reach the invoking user's ~/.fleet/kube).
func TestKubeRemovalReleaseIsWaitedFor(t *testing.T) {
	f := newKubeFixture(t)
	m := f.svc.kube
	m.mu.Lock()
	m.images = &kubeImages{manager: &kubeimage.Manager{IndexPath: state.KubeImagesPath}}
	m.mu.Unlock()
	id := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	index := state.KubeSeedCachesPath(id)
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !m.waitCleanups(ctx) {
		t.Fatal("the removal's release did not end")
	}
	if _, err := os.Stat(state.KubeClusterDir(id)); !os.IsNotExist(err) {
		t.Fatalf("the removed connection's directory is still there once its release ended (stat: %v)", err)
	}
}

// The serve loop's end ends the collections it started, and waitCleanups
// sees them through: the shutdown's last step.
func TestKubeCleanupsEndWithTheServeLoop(t *testing.T) {
	m := newKubeManager(newService())
	serve, stop := context.WithCancel(context.Background())
	defer stop()
	m.mu.Lock()
	m.ctx = serve
	m.mu.Unlock()
	ended := make(chan error, 1)
	m.cleanup(func(ctx context.Context) {
		<-ctx.Done()
		ended <- ctx.Err()
	})
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if m.waitCleanups(short) {
		t.Fatal("waitCleanups returned while a collection still ran")
	}
	stop()
	long, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !m.waitCleanups(long) {
		t.Fatal("a collection outlived the serve loop")
	}
	if err := <-ended; err != context.Canceled {
		t.Fatalf("the collection ended with %v, want the serve loop's cancel", err)
	}
}
