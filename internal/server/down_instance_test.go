package server

import (
	"errors"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// downRecorder records Down calls; any other Backend method panics on the nil
// embedded interface.
type downRecorder struct {
	backend.Backend
	downs []string
}

func (d *downRecorder) Down(containerID string) error {
	d.downs = append(d.downs, containerID)
	return nil
}

// orphanDowner is a backend that can also tear down by key.
type orphanDowner struct {
	downRecorder
	byKey int
	err   error
}

func (o *orphanDowner) DownByKey() error {
	o.byKey++
	return o.err
}

func TestDownInstanceRoutesByContainerIDAndOrphanDowner(t *testing.T) {
	// A provisioned instance goes through Down, even on an OrphanDowner.
	od := &orphanDowner{}
	if err := downInstance(od, &fleet.Instance{Name: "i", ContainerID: "c1"}); err != nil {
		t.Fatalf("downInstance: %v", err)
	}
	if len(od.downs) != 1 || od.downs[0] != "c1" || od.byKey != 0 {
		t.Fatalf("provisioned instance: Down %v, DownByKey %d; want Down(c1) only", od.downs, od.byKey)
	}

	// No ContainerID: an OrphanDowner removes what the failed create left.
	od = &orphanDowner{err: errors.New("cluster unreachable")}
	if err := downInstance(od, &fleet.Instance{Name: "i", Status: fleet.StatusFailed}); err == nil || err.Error() != "cluster unreachable" {
		t.Fatalf("orphan teardown error = %v, want the DownByKey error surfaced for the warning", err)
	}
	if od.byKey != 1 || len(od.downs) != 0 {
		t.Fatalf("orphan teardown: DownByKey %d, Down %v; want DownByKey once", od.byKey, od.downs)
	}

	// No ContainerID on any other backend: nothing to remove, nothing called.
	plain := &downRecorder{}
	if err := downInstance(plain, &fleet.Instance{Name: "i", Status: fleet.StatusFailed}); err != nil {
		t.Fatalf("downInstance: %v", err)
	}
	if len(plain.downs) != 0 {
		t.Fatalf("Down called %v for an instance with no container", plain.downs)
	}
}
