//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// logs.go reads an instance container's log from the API server for fleetd:
// what the pod printed across the runs the cluster will soon drop (a crashed
// run before parking, the previous container after a restart, a terminating
// pod), which fleetd saves to container.log so `fleet logs` shows every run
// as `docker logs` does.

// ReadContainerLog copies the dev container's log on connection clusterID to
// w: the running container's, or (previous) the one it replaced. It is one
// pods/log read through the pool's own clientset.
func (p *Pool) ReadContainerLog(ctx context.Context, clusterID, pod string, previous bool, w io.Writer) error {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return err
	}
	return kubeexec.PodLogsClient(ctx, cs, kubeexec.PodLogsRequest{
		Namespace: c.Namespace(), Pod: pod, Container: DevContainer, Previous: previous,
	}, w)
}

// ReadRollingLog reads back pod-init's rolling copy of the instance's output
// from the instance disk (podseed.LogDir, root-owned), run by run. It is the
// node-failure fallback: when the cluster recreates the instance's pod on a
// new node, the API server has no log of the runs on the lost node, but the
// rolling copy survived on the persisted disk. It streams the three rolling
// files on the bulk session as root; a missing file is empty.
func (b *Backend) ReadRollingLog(ctx context.Context) ([]podinit.RunOutput, error) {
	s := b.liveSession("")
	if s == nil {
		var err error
		s, err = b.agent(ctx, false)
		if err != nil {
			return nil, err
		}
	}
	bulk, err := s.Bulk(ctx)
	if err != nil {
		return nil, fmt.Errorf("the pod's bulk session: %w", err)
	}
	var f podinit.RollingFiles
	dst := []*[]byte{&f.Index, &f.Prev, &f.Cur}
	for i, name := range podinit.RollingDirFiles {
		data, err := readPodFile(ctx, bulk, podseed.LogDir+"/"+name)
		if err != nil {
			return nil, err
		}
		*dst[i] = data
	}
	return podinit.ParseRolling(f).Runs(), nil
}

// readPodFile streams a root-owned pod file into memory; a missing file is
// nil, no error.
func readPodFile(ctx context.Context, c *podwire.Client, path string) ([]byte, error) {
	r, err := c.OpenFile(ctx, podwire.FileReadHeader{Path: path, Class: execas.ClassRoot, Max: rollingReadCap})
	if err != nil {
		if podwire.IsCode(err, podwire.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// rollingReadCap bounds one rolling segment read (each is at most half the
// rolling cap; podinit.DefaultLogMax is 10 MB, so 8 MB is ample headroom).
const rollingReadCap = 8 << 20
