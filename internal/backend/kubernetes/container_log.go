//go:build !nok8s

package kubernetes

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// container_log.go is the instance's saved runtime log,
// ~/.fleet/logs/<fleet>/<instance>/container.log: what its container printed
// across the runs the cluster has dropped, kept the way `docker logs` keeps a
// container's across stop/start. fleetd appends to it as the pool's informers
// see a run end (the server's kube_logs) and when a start reads back
// pod-init's rolling copy (lifecycle); it is removed with the instance. One
// append path, capped and serialized, for both writers.

// ContainerLogMax bounds one instance's saved log (the plan's 10 MB): older
// whole lines are dropped past it.
const ContainerLogMax = 10 << 20

// containerLogMu serializes appends across the server's run-end saver and the
// backend's rolling read-back (both in-process).
var containerLogMu sync.Mutex

// AppendContainerLog appends data to the instance's container.log (0700 dirs,
// 0600 file), keeping it under ContainerLogMax.
func AppendContainerLog(fleetName, instance string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	path := fleetpaths.ContainerLogPath(fleetName, instance)
	containerLogMu.Lock()
	defer containerLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	all := capLog(append(old, data...), ContainerLogMax)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, all, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveContainerLog removes the instance's saved log (its teardown).
func RemoveContainerLog(fleetName, instance string) {
	containerLogMu.Lock()
	defer containerLogMu.Unlock()
	_ = os.Remove(fleetpaths.ContainerLogPath(fleetName, instance))
}

// capLog trims data to at most max bytes, dropping whole leading lines so the
// front is never cut mid-line.
func capLog(data []byte, max int) []byte {
	if len(data) <= max {
		return data
	}
	data = data[len(data)-max:]
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i+1 < len(data) {
		data = data[i+1:]
	}
	return data
}
