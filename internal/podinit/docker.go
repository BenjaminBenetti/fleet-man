package podinit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// docker.go is the configuration of a docker engine the dev container runs
// itself (docker-in-docker), written into its daemon.json before the start
// script starts it: the pod network's MTU, the connection's address pools,
// and, where pod-init laid out cgroups, a cgroup parent inside the user
// cgroup, so the engine's containers count against the pod and freeze with
// the user's processes instead of landing at the root of the node's
// hierarchy (a privileged container shares the node's cgroup namespace on
// containerd and CRI-O).

// DefaultDockerDaemonJSON is where dockerd reads its config.
const DefaultDockerDaemonJSON = "/etc/docker/daemon.json"

// Paths pod-init reads the pod network and the image from; tests point them
// elsewhere.
var (
	procNetRoute = "/proc/net/route"
	sysClassNet  = "/sys/class/net"
	// dockerdDirs are where an image installs dockerd.
	dockerdDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	// dindInit is the docker-in-docker feature's entrypoint, which passes
	// its dockerDefaultAddressPool option to dockerd as a flag.
	dindInit = "/usr/local/share/docker-init.sh"
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

// dockerdInstalled reports whether the image has a docker engine to
// configure: a dockerd in one of the usual dirs, or a daemon.json at file.
// A privileged image without docker gets no /etc/docker of fleet's.
func dockerdInstalled(file string) bool {
	if _, err := os.Stat(file); err == nil {
		return true
	}
	for _, dir := range dockerdDirs {
		if fi, err := os.Stat(filepath.Join(dir, "dockerd")); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

// dindPoolFlag matches the docker-in-docker feature's entrypoint line that
// carries a dockerDefaultAddressPool option.
var dindPoolFlag = regexp.MustCompile(`(?m)^DOCKER_DEFAULT_ADDRESS_POOL=\S`)

// dindSetsPools reports whether the image's docker-in-docker entrypoint
// starts dockerd with --default-address-pool: dockerd refuses to start when
// a flag and its config both set one.
func dindSetsPools() bool {
	data, err := os.ReadFile(dindInit)
	return err == nil && dindPoolFlag.Match(data)
}

// dockerConfig is what pod-init writes into a daemon.json; the zero value of
// a field is a setting fleet no longer makes (one it made goes, mergeDockerConfig).
type dockerConfig struct {
	mtu          int
	pools        []AddressPool
	cgroupParent string
	// keep are keys whose value fleet could not tell this time (the pod's
	// MTU unreadable): whatever the config has stays.
	keep []string
}

// configureDocker writes d (with the pod network's MTU when it names none)
// and the cgroup parent below layout into the image's daemon.json, when the
// image has a docker engine. It returns warnings; none of them stops the
// start script.
func configureDocker(d *Docker, layout *Layout) []string {
	file := d.DaemonJSON
	if file == "" {
		file = DefaultDockerDaemonJSON
	}
	if !dockerdInstalled(file) {
		return nil
	}
	var warns []string
	cfg := dockerConfig{mtu: d.MTU, pools: d.AddressPools}
	if cfg.mtu == 0 {
		mtu, err := podMTU()
		if err != nil {
			warns = append(warns, fmt.Sprintf("docker mtu: %v", err))
			cfg.keep = append(cfg.keep, keyMTU)
		}
		cfg.mtu = mtu
	}
	if len(cfg.pools) > 0 && dindSetsPools() {
		warns = append(warns, "docker address pools: the image's docker-in-docker feature sets dockerDefaultAddressPool, which wins over the connection's dind_address_pool")
		cfg.pools = nil
	}
	if layout != nil {
		cfg.cgroupParent = layout.DockerParent()
	}
	if _, warn, err := mergeDockerConfig(file, cfg); err != nil {
		warns = append(warns, fmt.Sprintf("docker config: %v", err))
	} else if warn != "" {
		warns = append(warns, warn)
	}
	return warns
}

// The daemon.json keys pod-init sets.
const (
	keyMTU          = "mtu"
	keyAddressPools = "default-address-pools"
	keyCgroupParent = "cgroup-parent"
)

// dockerSetRecord is what fleet set in a daemon.json, kept beside it
// (setRecordFile; dockerd refuses keys it does not know, so not in it): each
// key's value as fleet wrote it, and the value it had before, so a setting
// fleet no longer makes (a cleared dind_address_pool) goes again instead of
// staying for good, and the image's or user's own value comes back. A key
// whose value someone changed since fleet set it is theirs and stays.
type dockerSetRecord struct {
	V int `json:"v"`
	// Set are fleet's values; Prior the values the config had before fleet
	// set them (a key it lacked has none).
	Set   map[string]any `json:"set,omitempty"`
	Prior map[string]any `json:"prior,omitempty"`
}

// setRecordFile is where the record of what fleet set in file is kept.
func setRecordFile(file string) string { return file + ".fleet-set" }

// mergeDockerConfig sets cfg's settings in a dockerd config (creating it),
// keeping every other key, and takes out the ones fleet set before that cfg
// no longer makes (back to what they were before fleet set them), and
// reports whether it changed anything. A config that is not a JSON object is
// left alone, with an error: dockerd would refuse it anyway, and overwriting
// it would lose the user's settings. A config that picks the systemd cgroup
// driver keeps its own cgroup parent (a systemd slice), with a warning.
func mergeDockerConfig(file string, cfg dockerConfig) (changed bool, warn string, err error) {
	conf := map[string]any{}
	data, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, "", err
	case len(bytes.TrimSpace(data)) == 0:
	default:
		if err := json.Unmarshal(data, &conf); err != nil {
			return false, "", fmt.Errorf("%s is not a JSON object, left as it is: %w", file, err)
		}
		if conf == nil {
			// A JSON null: no settings to keep (and a nil map to write
			// into would panic PID 1).
			conf = map[string]any{}
		}
	}
	want := map[string]any{}
	if cfg.mtu > 0 {
		want[keyMTU] = float64(cfg.mtu)
	}
	if len(cfg.pools) > 0 {
		var pools []any
		for _, p := range cfg.pools {
			pools = append(pools, map[string]any{"base": p.Base, "size": float64(p.Size)})
		}
		want[keyAddressPools] = pools
	}
	if cfg.cgroupParent != "" {
		if systemdDriver(conf) {
			warn = fmt.Sprintf("docker cgroups: %s picks the systemd cgroup driver, so its containers' cgroups stay outside %s", file, UserCgroup)
		} else {
			want[keyCgroupParent] = cfg.cgroupParent
		}
	}
	rec := readSetRecord(setRecordFile(file))
	before := rec.clone()
	for k, was := range rec.Set {
		if _, still := want[k]; still || slices.Contains(cfg.keep, k) {
			continue
		}
		if reflect.DeepEqual(conf[k], was) {
			if prior, ok := rec.Prior[k]; ok {
				conf[k] = prior
			} else {
				delete(conf, k)
			}
			changed = true
		}
		delete(rec.Set, k)
		delete(rec.Prior, k)
	}
	for k, v := range want {
		if reflect.DeepEqual(conf[k], v) {
			continue
		}
		if _, ours := rec.Set[k]; !ours {
			if have, ok := conf[k]; ok {
				rec.Prior[k] = have
			}
		}
		rec.Set[k] = v
		conf[k] = v
		changed = true
	}
	// The record goes first: a config written without it would keep a
	// setting of fleet's for good.
	if !reflect.DeepEqual(rec, before) {
		if err := writeSetRecord(setRecordFile(file), rec); err != nil {
			return false, warn, fmt.Errorf("record what fleet sets in %s: %w", file, err)
		}
	}
	if !changed {
		return false, warn, nil
	}
	out, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return false, warn, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return false, warn, err
	}
	tmp := file + ".fleet-tmp"
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(tmp, append(out, '\n'), mode); err != nil {
		return false, warn, err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return false, warn, err
	}
	return true, warn, nil
}

// readSetRecord is the record at file; an empty one when there is none or
// it cannot be read (fleet then takes back nothing it set before).
func readSetRecord(file string) dockerSetRecord {
	rec := dockerSetRecord{V: 1}
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &rec)
	}
	if rec.Set == nil {
		rec.Set = map[string]any{}
	}
	if rec.Prior == nil {
		rec.Prior = map[string]any{}
	}
	return rec
}

func (r dockerSetRecord) clone() dockerSetRecord {
	return dockerSetRecord{V: r.V, Set: maps.Clone(r.Set), Prior: maps.Clone(r.Prior)}
}

// writeSetRecord writes rec at file (by rename), or removes the file when
// rec holds nothing.
func writeSetRecord(file string, rec dockerSetRecord) error {
	if len(rec.Set) == 0 && len(rec.Prior) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	rec.V = 1
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// systemdDriver reports a dockerd config whose exec-opts pick the systemd
// cgroup driver, whose cgroup parent must be a slice.
func systemdDriver(conf map[string]any) bool {
	opts, _ := conf["exec-opts"].([]any)
	for _, o := range opts {
		if s, ok := o.(string); ok && strings.ReplaceAll(s, " ", "") == "native.cgroupdriver=systemd" {
			return true
		}
	}
	return false
}
