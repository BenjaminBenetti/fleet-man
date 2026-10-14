//go:build !nok8s

package kubernetes

import (
	"fmt"
	"testing"
	"time"

	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// BenchmarkStatusOf300Instances is one live-status pass of the daemon's
// poller (runtime_poller.go liveStatusPass, every 60s while a TUI is open)
// over 300 running instances: each Status reads and decodes its instance
// record from disk (replacedGeneration) before the pool's status table
// answers. The merge checklist reads its time per op as the pass's cost.
func BenchmarkStatusOf300Instances(b *testing.B) {
	b.Setenv("HOME", b.TempDir())
	rec := fleet.KubeCluster{ID: handleCluster, Context: "ctx", Namespace: handleNS}
	cs := fake.NewClientset()
	pool := NewPool(PoolOptions{
		Records:   func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Environ:   func() []string { return []string{"PATH=/usr/bin:/bin"} },
		Clientset: func(*Cluster) (kclient.Interface, error) { return cs, nil },
		Owner:     func() (string, error) { return handleOwner, nil },
	})
	b.Cleanup(pool.Close)

	const n = 300
	type probe struct {
		b  *Backend
		id string
	}
	probes := make([]probe, 0, n)
	for i := range n {
		name := fmt.Sprintf("web-%03d", i)
		key := names.InstanceKey("daemon1", "alpha", name, time.Unix(1760000000, 0))
		sts := names.Instance("alpha", name, key)
		r := InstanceRecord{
			Cluster: handleCluster, Namespace: handleNS, Key: key, Fleet: "alpha", Instance: name,
			StatefulSet: sts, StatefulSetUID: "uid-" + sts, Gen: 1, SpecHash: "sha256:" + key,
			Image:        "fleet.invalid/abcdef/alpha@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigests: []string{"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"},
			RemoteUser:   "vscode", WorkspaceFolder: "/workspaces/alpha", Created: time.Unix(1760000000, 0).UTC(),
			Ports: []backend.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3000, ContainerPort: 3000}},
			Share: &ShareRecord{Mode: "single-node", Claim: "fs-alpha-1a2b3c4d-share", UID: 1000, GID: 1000, Roots: []podshare.SpokeRoot{
				{Name: ".claude"}, {Name: "files", Only: []string{".claude.json", "claude-settings.local.json"}}, {Name: ".codex"}, {Name: ".config/gh"},
				{Name: ".mnt", Reserved: []string{"opt/data"}},
			}},
		}
		if err := SaveInstanceRecord(r); err != nil {
			b.Fatal(err)
		}
		inst := &fleet.Instance{Name: name, Backend: fleet.BackendKubernetes, KubeCluster: handleCluster, KubeKey: key}
		probes = append(probes, probe{newHandle(inst, pool), ContainerID(handleCluster, handleNS, sts, 1)})
	}
	b.ResetTimer()
	for b.Loop() {
		for _, p := range probes {
			if got := p.b.Status(p.id); got == backend.LiveStatusMissing {
				b.Fatalf("%s reads missing", p.id)
			}
		}
	}
}
