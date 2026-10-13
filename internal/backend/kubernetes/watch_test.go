//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// A pod is read from the informers' cache only while the pods informer
// itself follows the cluster. After an outage the StatefulSet informer can
// answer again long before the pods one (each on its own backoff), and the
// pods cache then still holds the pod of before the outage: a pod replaced
// meanwhile verified from it would be vouched for as the pod that is gone.
func TestCacheObjectsReadAPodOnlyFromACurrentPodsInformer(t *testing.T) {
	live := &corev1.Pod{ObjectMeta: handleMeta(names.Pod(handleSTS()))}
	r := newHandleRig(t, live)
	c, err := r.pool.Cluster(handleCluster)
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.pool.watcherFor(c)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.synced:
	case <-time.After(10 * time.Second):
		t.Fatal("the informers never listed")
	}
	// The cache missed the pod's replacement: it holds the one that is gone.
	gone := live.DeepCopy()
	gone.UID = "uid-gone"
	if err := w.pods.GetIndexer().Update(gone); err != nil {
		t.Fatal(err)
	}
	objs, err := r.pool.objects(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	read := func() types.UID {
		t.Helper()
		pod, err := objs.Pod(ctx, handleNS, live.Name)
		if err != nil {
			t.Fatal(err)
		}
		return pod.UID
	}
	if got := read(); got != gone.UID {
		t.Fatalf("a current cache: read pod %s, want the cache's %s", got, gone.UID)
	}

	down := errors.New("dial tcp: connection refused")
	w.answered(ctx, podInformer, down)
	w.answered(ctx, setInformer, down)
	w.answered(ctx, setInformer, nil)
	if err := w.health(); err != nil {
		t.Fatalf("health with the StatefulSet informer back: %v, want the API server answering", err)
	}
	if got := read(); got != live.UID {
		t.Fatalf("pods informer still away: read pod %s, want the API server's %s", got, live.UID)
	}

	w.answered(ctx, podInformer, nil)
	if got := read(); got != gone.UID {
		t.Fatalf("pods informer back: read pod %s, want the cache's %s", got, gone.UID)
	}
}
