//go:build !nok8s

package backendutil

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	kubernetesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestNewForInstanceBindsTheKubernetesHandle: a Kubernetes record gets the
// Kubernetes handle, from both constructors, with the capabilities the
// daemon relies on (a control channel, so provisioning mounts no host
// control directory; teardown by key; supervision of its pod).
func TestNewForInstanceBindsTheKubernetesHandle(t *testing.T) {
	inst := &fleet.Instance{
		Name: "web", Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234", KubeKey: "abcd2345",
		WorkspaceDir: "/home/u/.fleet/workspaces/alpha/web/alpha", CreatedAt: time.Unix(1700000000, 0),
	}
	b := NewForInstance(inst, false)
	if _, ok := b.(*kubernetesbackend.Backend); !ok {
		t.Fatalf("NewForInstance(kubernetes) = %T", b)
	}
	for name, ok := range map[string]bool{
		"ControlChannel":   backend.HasControlChannel(b),
		"OrphanDowner":     is[backend.OrphanDowner](b),
		"UpContexter":      is[backend.UpContexter](b),
		"RebuildContexter": is[backend.RebuildContexter](b),
		"RestartAware":     is[backend.RestartAware](b),
		"PostStarter":      is[backend.PostStarter](b),
		"TargetProvider":   is[kubernetesbackend.TargetProvider](b),
	} {
		if !ok {
			t.Errorf("the Kubernetes handle is not a %s", name)
		}
	}
	caps := CapabilitiesFor(inst)
	if _, ok := caps.(*kubernetesbackend.Backend); !ok || !backend.HasControlChannel(caps) {
		t.Errorf("CapabilitiesFor(kubernetes) = %T", caps)
	}
	if !caps.SupportsSharedMounts() || caps.SupportsClone() || !caps.SupportsRebuild() || caps.SupportsMicSink() {
		t.Errorf("Kubernetes capabilities: shared %v clone %v rebuild %v mic %v", caps.SupportsSharedMounts(), caps.SupportsClone(), caps.SupportsRebuild(), caps.SupportsMicSink())
	}
}

func is[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
