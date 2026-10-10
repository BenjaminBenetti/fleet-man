//go:build !nok8s

package kubernetes

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

// container_log.go is the instance's saved runtime log,
// ~/.fleet/logs/<fleet>/<instance>/container.log: what its container printed
// across the runs the cluster has dropped, kept the way `docker logs` keeps a
// container's across stop/start. fleetd appends to it as the pool's informers
// see a run end (the server's kube_logs) and when a start reads back
// pod-init's rolling copy; it is removed with the instance. One append path,
// capped and serialized, for both writers.
//
// Each saved run starts with a header naming it by its pod's uid and the dev
// container's restart count while it ran (RunHeader), the same run whichever
// writer saved it, so the file itself records what is saved: a run is never
// saved twice, across writers and across fleetd restarts (SavedRuns).

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
	return atomicfile.Write(path, capLog(append(old, data...), ContainerLogMax), 0o600)
}

// RemoveContainerLog removes the instance's saved log (its teardown), and
// its directory once that is empty.
func RemoveContainerLog(fleetName, instance string) {
	containerLogMu.Lock()
	defer containerLogMu.Unlock()
	path := fleetpaths.ContainerLogPath(fleetName, instance)
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path))
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

// RunKey names one run of an instance's dev container: its pod's uid and the
// container's restart count while it ran (the kubelet's restartCount; the
// pod's n-th start of pod-init, from 0).
func RunKey(podUID string, restart int32) string {
	return podUID + "/" + strconv.Itoa(int(restart))
}

// RunHeader is the line a saved run starts with in container.log: why it was
// saved, the run (RunKey's parts) and when.
func RunHeader(why, podUID string, restart int32, at time.Time) string {
	return fmt.Sprintf("===== %s run (pod %s restart %d) %s =====\n", why, podUID, restart, at.UTC().Format(time.RFC3339))
}

// runHeaderRE matches RunHeader's run.
var runHeaderRE = regexp.MustCompile(`^===== \S+ run \(pod (\S+) restart (\d+)\) `)

// SavedRuns is the runs (RunKey) the instance's container.log holds, read
// from its headers: what is saved already, whichever writer, whichever fleetd
// process, saved it. A missing file holds none.
func SavedRuns(fleetName, instance string) map[string]bool {
	containerLogMu.Lock()
	data, err := os.ReadFile(fleetpaths.ContainerLogPath(fleetName, instance))
	containerLogMu.Unlock()
	saved := map[string]bool{}
	if err != nil {
		return saved
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), ContainerLogMax)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("===== ")) {
			continue
		}
		if m := runHeaderRE.FindSubmatch(line); m != nil {
			if n, err := strconv.ParseInt(string(m[2]), 10, 32); err == nil {
				saved[RunKey(string(m[1]), int32(n))] = true
			}
		}
	}
	return saved
}

// EndedRollingRuns is the runs of pod-init's rolling copy that have ended
// and can be named (RunKey(PodUID, PodSeq)): the last run is the current one
// (pod-init marks a run as it starts, before the pod is ready), still running
// and on the live log, so it is left out, and so are runs without a pod uid.
func EndedRollingRuns(runs []podinit.RunOutput) []podinit.RunOutput {
	if len(runs) == 0 {
		return nil
	}
	var out []podinit.RunOutput
	for _, r := range runs[:len(runs)-1] {
		if r.PodUID != "" {
			out = append(out, r)
		}
	}
	return out
}
