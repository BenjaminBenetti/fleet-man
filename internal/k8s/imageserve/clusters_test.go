package imageserve

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestStateClustersKeepsCredentialsInTheRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	twenty := 20
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = append(k.Clusters,
			fleet.KubeCluster{ID: "kc-a", Context: "a", Namespace: "dev"},
			fleet.KubeCluster{ID: "kc-b", Context: "b", Namespace: "dev", Image: fleet.KubeImageSettings{UploadPacePercent: &twenty}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Now()}
	c := &StateClusters{Now: clock.now}

	got, ok := c.Lookup("kc-a")
	if !ok || got.Credential != "" || got.PacePercent != fleet.DefaultKubeUploadPacePercent {
		t.Fatalf("Lookup before any delivery: %+v, %v", got, ok)
	}
	if got, _ := c.Lookup("kc-b"); got.PacePercent != 20 {
		t.Fatalf("pace %d, want the connection's 20", got.PacePercent)
	}
	if _, ok := c.Lookup("kc-x"); ok {
		t.Fatal("an unknown connection was found")
	}

	cred, err := c.EnsureCredential("kc-a")
	if err != nil || len(cred) < 40 {
		t.Fatalf("EnsureCredential: %q, %v", cred, err)
	}
	if again, _ := c.EnsureCredential("kc-a"); again != cred {
		t.Fatal("a second EnsureCredential minted another credential")
	}
	// It is in the registry file (0600), on the record.
	k, _ := state.LoadKubeClusters()
	if k.Find("kc-a").Image.GatewayCredential != cred {
		t.Fatal("the credential was not saved on the record")
	}
	if fi, err := os.Stat(state.KubeClustersPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kube_clusters.json: %v %v", fi, err)
	}
	if got, _ := c.Lookup("kc-a"); got.Credential != cred {
		t.Fatalf("Lookup after EnsureCredential: %+v", got)
	}

	// Rotation: a new credential, the old one accepted for a while.
	next, err := c.RotateCredential("kc-a")
	if err != nil || next == cred {
		t.Fatalf("RotateCredential: %q, %v", next, err)
	}
	got, _ = c.Lookup("kc-a")
	if got.Credential != next || got.Previous != cred || !got.PreviousUntil.After(clock.t) {
		t.Fatalf("after rotation: %+v", got)
	}
	clock.advance(rotationGrace + time.Second)
	if got, _ := c.Lookup("kc-a"); got.Previous != "" {
		t.Fatalf("the previous credential outlived its grace: %+v", got)
	}

	// A removed connection is gone at the next read (within the TTL).
	_ = state.UpdateKubeClusters(func(k *state.KubeClusters) error { k.Remove("kc-a"); return nil })
	clock.advance(stateClustersTTL + time.Millisecond)
	if _, ok := c.Lookup("kc-a"); ok {
		t.Fatal("a removed connection still authenticates")
	}
	if _, err := c.EnsureCredential("kc-a"); err == nil || !strings.Contains(err.Error(), "kc-a") {
		t.Fatalf("EnsureCredential of a removed connection: %v", err)
	}
}
