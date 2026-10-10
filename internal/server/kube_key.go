package server

import (
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_key.go gives a new Kubernetes instance its key: names.InstanceKey of
// the daemon's id, the fleet, the instance and its creation time. The key is
// persisted on the CREATING record before the create job starts, so every
// object the job applies (each labelled with it) is findable from the
// record, even one a create that failed or was interrupted left behind.

// loadDaemonIdentity is owner.LoadIdentity (a test seam).
var loadDaemonIdentity = owner.LoadIdentity

// kubeKeyAttempts bounds the search for a key no record holds: a collision
// (40 bits) moves the creation time on by a microsecond and tries again.
const kubeKeyAttempts = 8

// assignKubeKey sets inst's KubeKey, moving its CreatedAt on (by a
// microsecond at a time) in the unlikely case the key another record holds
// comes out. Called under the state lock, before inst is added to st.
func assignKubeKey(st *state.State, fleetName string, inst *fleet.Instance) error {
	id, err := loadDaemonIdentity()
	if err != nil {
		return fmt.Errorf("the daemon's identity, which names its Kubernetes objects: %w", err)
	}
	for range kubeKeyAttempts {
		key := names.InstanceKey(id.DaemonID, fleetName, inst.Name, inst.CreatedAt)
		if !kubeKeyTaken(st, key) {
			inst.KubeKey = key
			return nil
		}
		inst.CreatedAt = inst.CreatedAt.Add(time.Microsecond)
	}
	return fmt.Errorf("no free Kubernetes instance key for %s/%s", fleetName, inst.Name)
}

// kubeKeyTaken reports whether a record in st holds key.
func kubeKeyTaken(st *state.State, key string) bool {
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.KubeKey == key {
				return true
			}
		}
	}
	return false
}
