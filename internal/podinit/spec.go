package podinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// DefaultSpecPath is where an instance pod finds pid1.json: the <sts>-spec
// Secret, mounted at /opt/fleet/spec.
const DefaultSpecPath = podseed.SpecDir + "/pid1.json"

// SpecVersion is the pid1.json version this fleet writes and reads.
const SpecVersion = 1

// DefaultLogMax is the rolling output file's size cap.
const DefaultLogMax = 10 << 20

const maxSpecSize = 1 << 20

// Cgroup layout modes.
const (
	// CgroupsAuto lays out fleet-agent and fleet-user wherever the cgroupfs
	// is writable (privileged pods).
	CgroupsAuto = "auto"
	// CgroupsOff leaves the container's cgroup alone.
	CgroupsOff = "off"
)

// Spec is pid1.json, rendered by the daemon from the captured `docker run`.
// Unknown fields are ignored, so an older pod binary boots a newer daemon's
// spec; a newer V is refused.
type Spec struct {
	V int `json:"v"`
	// Argv is what docker would have run as the container's process: the
	// entrypoint and command, the devcontainer CLI's start script.
	Argv []string `json:"argv"`
	// User is containerUser (`docker run -u`), user[:group]; "" is root.
	User string `json:"user,omitempty"`
	// WorkingDir is `docker run -w`, else the image's WORKDIR; "" is /.
	WorkingDir string `json:"working_dir,omitempty"`
	// Env is set over the container's environment for the start script:
	// variables the pod spec cannot carry as written.
	Env map[string]string `json:"env,omitempty"`
	// Init is `docker run --init`: forward every signal, as tini does.
	// Without it pod-init forwards only the signals the start script
	// catches, which is what the kernel does for a PID 1.
	Init bool `json:"init,omitempty"`
	// StopSignal is --stop-signal (or the image's STOPSIGNAL): what the
	// start script gets when the kubelet stops the pod with SIGTERM.
	StopSignal string `json:"stop_signal,omitempty"`
	// Rlimits are the --ulimit settings.
	Rlimits []Rlimit `json:"rlimits,omitempty"`
	// Hostname is --hostname, set when the pod may (CAP_SYS_ADMIN).
	Hostname string `json:"hostname,omitempty"`
	// DockerMTU writes the pod network's MTU into the image's dockerd
	// config for DinD.
	DockerMTU *DockerMTU `json:"docker_mtu,omitempty"`
	// BinarySHA256 is the fleet binary the spec was rendered for;
	// FallbackSHA256 the one before it. pod-init re-execs fleet-<sha> from
	// the bin dir when it is neither.
	BinarySHA256   string `json:"binary_sha256,omitempty"`
	FallbackSHA256 string `json:"fallback_sha256,omitempty"`
	// RemoteUser is whose home and shell the pod's uid gets when the image
	// has no passwd entry for it (capability tier C).
	RemoteUser string `json:"remote_user,omitempty"`
	// Cgroups is CgroupsAuto ("" too) or CgroupsOff.
	Cgroups string `json:"cgroups,omitempty"`
	// MemoryMargin is how far below the container's memory limit fleet-user's
	// memory.max sits, in bytes; 0 picks one from the limit.
	MemoryMargin int64 `json:"memory_margin,omitempty"`
	// LogDir and LogMax place and cap the rolling copy of the output; ""
	// is podseed.LogDir, 0 is DefaultLogMax.
	LogDir string `json:"log_dir,omitempty"`
	LogMax int64  `json:"log_max,omitempty"`
}

// Rlimit is one --ulimit: name=soft[:hard], -1 for unlimited.
type Rlimit struct {
	Name string `json:"name"`
	Soft int64  `json:"soft"`
	Hard int64  `json:"hard"`
}

// DockerMTU configures the DinD daemon's MTU.
type DockerMTU struct {
	// MTU to write; 0 reads the pod network's (the default route's
	// interface).
	MTU int `json:"mtu,omitempty"`
	// DaemonJSON is the dockerd config to merge it into; "" is
	// /etc/docker/daemon.json.
	DaemonJSON string `json:"daemon_json,omitempty"`
}

// rlimitNames are the --ulimit names docker accepts.
var rlimitNames = map[string]bool{
	"core": true, "cpu": true, "data": true, "fsize": true, "locks": true, "memlock": true,
	"msgqueue": true, "nice": true, "nofile": true, "nproc": true, "rss": true, "rtprio": true,
	"rttime": true, "sigpending": true, "stack": true, "as": true,
}

// signalNames are the signals a stop signal may name: Linux's, by name, since
// the spec is rendered anywhere but read in a linux pod.
var signalNames = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "BUS": 7, "FPE": 8,
	"KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15,
	"STKFLT": 16, "CHLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22,
	"URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29,
	"POLL": 29, "PWR": 30, "SYS": 31,
}

// Linux's real-time signal range, as glibc and docker number it.
const (
	sigRTMin = 34
	sigRTMax = 64
)

var rtSignal = regexp.MustCompile(`^RTMIN(\+([0-9]+))?$|^RTMAX(-([0-9]+))?$`)

// SignalNumber is the Linux number of a signal named as docker takes it:
// "SIGTERM", "TERM", "15", "SIGRTMIN+3" (case-insensitive).
func SignalNumber(name string) (int, error) {
	s := strings.ToUpper(strings.TrimSpace(name))
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > sigRTMax {
			return 0, fmt.Errorf("signal %d is out of range", n)
		}
		return n, nil
	}
	s = strings.TrimPrefix(s, "SIG")
	if n, ok := signalNames[s]; ok {
		return n, nil
	}
	if m := rtSignal.FindStringSubmatch(s); m != nil {
		n := sigRTMin
		if m[2] != "" {
			off, _ := strconv.Atoi(m[2])
			n += off
		} else if strings.HasPrefix(s, "RTMAX") {
			n = sigRTMax
			if m[4] != "" {
				off, _ := strconv.Atoi(m[4])
				n -= off
			}
		}
		if n >= sigRTMin && n <= sigRTMax {
			return n, nil
		}
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}

// LoadSpec reads and checks pid1.json at file.
func LoadSpec(file string) (Spec, error) {
	f, err := os.Open(file)
	if err != nil {
		return Spec{}, fmt.Errorf("read pid1 spec: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSpecSize+1))
	if err != nil {
		return Spec{}, fmt.Errorf("read pid1 spec %s: %w", file, err)
	}
	if len(data) > maxSpecSize {
		return Spec{}, fmt.Errorf("pid1 spec %s is larger than %d bytes", file, maxSpecSize)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse pid1 spec %s: %w", file, err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("pid1 spec %s: %w", file, err)
	}
	return spec, nil
}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the spec can be run as written.
func (s Spec) Validate() error {
	if s.V != SpecVersion {
		return fmt.Errorf("pid1 spec version %d, this fleet runs %d", s.V, SpecVersion)
	}
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return errors.New("no start command")
	}
	for _, a := range s.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("the start command has a NUL byte")
		}
	}
	if strings.ContainsAny(s.User, "\x00\n") {
		return fmt.Errorf("user %q is not usable", s.User)
	}
	if s.WorkingDir != "" && (!path.IsAbs(s.WorkingDir) || strings.IndexByte(s.WorkingDir, 0) >= 0) {
		return fmt.Errorf("working dir %q is not absolute", s.WorkingDir)
	}
	for name, value := range s.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("env %q cannot be set", name)
		}
	}
	if s.StopSignal != "" {
		if _, err := SignalNumber(s.StopSignal); err != nil {
			return fmt.Errorf("stop signal: %w", err)
		}
	}
	for _, r := range s.Rlimits {
		if !rlimitNames[r.Name] {
			return fmt.Errorf("unknown ulimit %q", r.Name)
		}
		if r.Soft < -1 || r.Hard < -1 || (r.Hard != -1 && (r.Soft == -1 || r.Soft > r.Hard)) {
			return fmt.Errorf("ulimit %s: soft %d is above hard %d", r.Name, r.Soft, r.Hard)
		}
	}
	if s.Hostname != "" && (len(s.Hostname) > 64 || strings.ContainsAny(s.Hostname, "/\x00 \n")) {
		return fmt.Errorf("hostname %q is not usable", s.Hostname)
	}
	if m := s.DockerMTU; m != nil {
		if m.MTU < 0 || (m.MTU != 0 && m.MTU < 576) || m.MTU > 65535 {
			return fmt.Errorf("docker mtu %d is out of range", m.MTU)
		}
		if m.DaemonJSON != "" && !path.IsAbs(m.DaemonJSON) {
			return fmt.Errorf("docker daemon config %q is not absolute", m.DaemonJSON)
		}
	}
	for name, sha := range map[string]string{"binary_sha256": s.BinarySHA256, "fallback_sha256": s.FallbackSHA256} {
		if sha != "" && !hexSHA.MatchString(sha) {
			return fmt.Errorf("%s %q is not a sha256", name, sha)
		}
	}
	if strings.ContainsAny(s.RemoteUser, ":\x00\n") {
		return fmt.Errorf("remote user %q is not usable", s.RemoteUser)
	}
	switch s.Cgroups {
	case "", CgroupsAuto, CgroupsOff:
	default:
		return fmt.Errorf("unknown cgroups mode %q", s.Cgroups)
	}
	if s.MemoryMargin < 0 || s.LogMax < 0 {
		return errors.New("memory margin and log max cannot be negative")
	}
	if s.LogDir != "" && !path.IsAbs(s.LogDir) {
		return fmt.Errorf("log dir %q is not absolute", s.LogDir)
	}
	return nil
}
