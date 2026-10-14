//go:build !nok8s

package preflight

import "time"

// BoundProbes caps the deep test's waits on pods that may never run (the
// network probe, the two-pod probe and the multi-node storage tests) at d,
// for the daemon's tests on a fake cluster: no probe pod runs there, so
// each would be waited out, minutes in all. The pull probe keeps its bound
// (a fake can finish it). restore puts the bounds back; neither is safe
// while a run is under way.
func BoundProbes(d time.Duration) (restore func()) {
	vars := []*time.Duration{
		&networkProbeTimeout,
		&sharePodsTimeout, &sharePodsStart, &sharePodsMount,
		&storageBatchTimeout, &storageTestStart, &storageTestStartLarge, &storageTestMount, &storageTestRun,
	}
	old := make([]time.Duration, len(vars))
	for i, v := range vars {
		old[i] = *v
		*v = min(*v, d)
	}
	return func() {
		for i, v := range vars {
			*v = old[i]
		}
	}
}
