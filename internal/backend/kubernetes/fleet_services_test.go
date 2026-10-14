//go:build !nok8s

package kubernetes

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
)

// The handle names the fleet's headless Service, the one the instance's
// StatefulSet is rendered with, in the instance's namespace: from its
// container id, else fleetd's record of it; an unbound record names none.
func TestPeerDomain(t *testing.T) {
	r := newHandleRig(t)
	ident, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	wantSvc := names.FleetObject("alpha", names.FleetHash(ident.DaemonID, handleCluster, "alpha"), names.SuffixNet)

	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, "team-ns", handleSTS(), 1)
	svc, ns, ok := newHandle(inst, r.pool).PeerDomain()
	if !ok || svc != wantSvc || ns != "team-ns" {
		t.Fatalf("peer domain %q %q %v, want %q in team-ns", svc, ns, ok, wantSvc)
	}

	// While creating: no container id yet, fleetd's record says where.
	inst = r.inst()
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web", StatefulSet: handleSTS(), Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if svc, ns, ok := newHandle(inst, r.pool).PeerDomain(); !ok || svc != wantSvc || ns != handleNS {
		t.Fatalf("peer domain while creating %q %q %v", svc, ns, ok)
	}

	if _, _, ok := newHandle(&fleet.Instance{Name: "web", Backend: fleet.BackendKubernetes}, r.pool).PeerDomain(); ok {
		t.Fatal("an unbound record named a peer domain")
	}
}
