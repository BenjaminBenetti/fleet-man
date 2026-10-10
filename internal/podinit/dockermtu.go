package podinit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultDockerDaemonJSON is where dockerd reads its config.
const DefaultDockerDaemonJSON = "/etc/docker/daemon.json"

// Paths pod-init reads the pod network from; tests point them elsewhere.
var (
	procNetRoute = "/proc/net/route"
	sysClassNet  = "/sys/class/net"
)

// defaultRouteIface is the interface of the IPv4 default route in a
// /proc/net/route table, "" without one.
func defaultRouteIface(table []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// podMTU is the pod network's MTU: the default route's interface's, else
// eth0's.
func podMTU() (int, error) {
	iface := "eth0"
	if table, err := os.ReadFile(procNetRoute); err == nil {
		if i := defaultRouteIface(table); i != "" {
			iface = i
		}
	}
	data, err := os.ReadFile(filepath.Join(sysClassNet, iface, "mtu"))
	if err != nil {
		return 0, fmt.Errorf("read %s's mtu: %w", iface, err)
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || mtu <= 0 {
		return 0, fmt.Errorf("%s's mtu %q is not a number", iface, data)
	}
	return mtu, nil
}

// mergeDockerMTU sets "mtu" in a dockerd config (creating it), keeping every
// other key, and reports whether it changed anything. A config that is not a
// JSON object is left alone, with an error: dockerd would refuse it anyway,
// and overwriting it would lose the user's settings.
func mergeDockerMTU(file string, mtu int) (bool, error) {
	cfg := map[string]any{}
	data, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	case len(bytes.TrimSpace(data)) == 0:
	default:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return false, fmt.Errorf("%s is not a JSON object, left as it is: %w", file, err)
		}
	}
	if cur, ok := cfg["mtu"].(float64); ok && int(cur) == mtu {
		return false, nil
	}
	cfg["mtu"] = mtu
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return false, err
	}
	tmp := file + ".fleet-tmp"
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(tmp, append(out, '\n'), mode); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
