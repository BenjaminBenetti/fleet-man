package kubeshim

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// runFlagSpecs is every option of `docker run` and `docker create` (docker CLI
// 29, cli/command/container/opts.go and run.go), the hidden aliases and the
// platform-specific ones included. Only the grammar lives here: which options
// take a value. What each one means on a cluster is the pod-spec translator's
// table, keyed by the same canonical names (RunFlagNames).
var runFlagSpecs = []flagSpec{
	{name: "add-host", value: true},
	{name: "annotation", value: true},
	{name: "attach", short: "a", value: true},
	{name: "blkio-weight", value: true},
	{name: "blkio-weight-device", value: true},
	{name: "cap-add", value: true},
	{name: "cap-drop", value: true},
	{name: "cgroup-parent", value: true},
	{name: "cgroupns", value: true},
	{name: "cidfile", value: true},
	{name: "cpu-count", value: true},
	{name: "cpu-percent", value: true},
	{name: "cpu-period", value: true},
	{name: "cpu-quota", value: true},
	{name: "cpu-rt-period", value: true},
	{name: "cpu-rt-runtime", value: true},
	{name: "cpu-shares", short: "c", value: true},
	{name: "cpus", value: true},
	{name: "cpuset-cpus", value: true},
	{name: "cpuset-mems", value: true},
	{name: "detach", short: "d"},
	{name: "detach-keys", value: true},
	{name: "device", value: true},
	{name: "device-cgroup-rule", value: true},
	{name: "device-read-bps", value: true},
	{name: "device-read-iops", value: true},
	{name: "device-write-bps", value: true},
	{name: "device-write-iops", value: true},
	{name: "disable-content-trust"},
	{name: "dns", value: true},
	{name: "dns-opt", value: true, canonical: "dns-option"},
	{name: "dns-option", value: true},
	{name: "dns-search", value: true},
	{name: "domainname", value: true},
	{name: "entrypoint", value: true},
	{name: "env", short: "e", value: true},
	{name: "env-file", value: true},
	{name: "expose", value: true},
	{name: "gpus", value: true},
	{name: "group-add", value: true},
	{name: "health-cmd", value: true},
	{name: "health-interval", value: true},
	{name: "health-retries", value: true},
	{name: "health-start-interval", value: true},
	{name: "health-start-period", value: true},
	{name: "health-timeout", value: true},
	{name: "help"},
	{name: "hostname", short: "h", value: true},
	{name: "init"},
	{name: "interactive", short: "i"},
	{name: "io-maxbandwidth", value: true},
	{name: "io-maxiops", value: true},
	{name: "ip", value: true},
	{name: "ip6", value: true},
	{name: "ipc", value: true},
	{name: "isolation", value: true},
	{name: "kernel-memory", value: true},
	{name: "label", short: "l", value: true},
	{name: "label-file", value: true},
	{name: "link", value: true},
	{name: "link-local-ip", value: true},
	{name: "log-driver", value: true},
	{name: "log-opt", value: true},
	{name: "mac-address", value: true},
	{name: "memory", short: "m", value: true},
	{name: "memory-reservation", value: true},
	{name: "memory-swap", value: true},
	{name: "memory-swappiness", value: true},
	{name: "mount", value: true},
	{name: "name", value: true},
	{name: "net", value: true, canonical: "network"},
	{name: "net-alias", value: true, canonical: "network-alias"},
	{name: "network", value: true},
	{name: "network-alias", value: true},
	{name: "no-healthcheck"},
	{name: "oom-kill-disable"},
	{name: "oom-score-adj", value: true},
	{name: "pid", value: true},
	{name: "pids-limit", value: true},
	{name: "platform", value: true},
	{name: "privileged"},
	{name: "publish", short: "p", value: true},
	{name: "publish-all", short: "P"},
	{name: "pull", value: true},
	{name: "quiet", short: "q"},
	{name: "read-only"},
	{name: "restart", value: true},
	{name: "rm"},
	{name: "runtime", value: true},
	{name: "security-opt", value: true},
	{name: "shm-size", value: true},
	{name: "sig-proxy"},
	{name: "stop-signal", value: true},
	{name: "stop-timeout", value: true},
	{name: "storage-opt", value: true},
	{name: "sysctl", value: true},
	{name: "tmpfs", value: true},
	{name: "tty", short: "t"},
	{name: "ulimit", value: true},
	{name: "umask", value: true},
	{name: "use-api-socket"},
	{name: "user", short: "u", value: true},
	{name: "userns", value: true},
	{name: "uts", value: true},
	{name: "volume", short: "v", value: true},
	{name: "volume-driver", value: true},
	{name: "volumes-from", value: true},
	{name: "workdir", short: "w", value: true},
}

var runFlags = newFlagSet(runFlagSpecs)

// RunFlagNames returns the canonical name of every `docker run` option the
// shim can parse, sorted. The pod-spec translator's flag table must cover each
// one (map, error or accepted delta), so a test there can assert it against
// this list.
func RunFlagNames() []string {
	seen := make(map[string]bool, len(runFlagSpecs))
	var names []string
	for _, s := range runFlagSpecs {
		if n := s.canonicalName(); !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// RunArgs is a `docker run` (or `docker create`) argv after the verb, parsed:
// the options in order, the image, and the command that follows it.
type RunArgs struct {
	Flags []Flag   `json:"flags"`
	Image string   `json:"image"`
	Cmd   []string `json:"cmd,omitempty"`
}

// ErrNoImage is a run with no image argument.
var ErrNoImage = errors.New("docker run without an image")

// ParseRun parses a `docker run` argv after the verb. An option the shim does
// not know fails it with an *UnknownFlagError, since without knowing whether
// it takes a value nothing after it can be read.
func ParseRun(args []string) (*RunArgs, error) {
	flags, rest, err := runFlags.parse(args)
	if err != nil {
		return nil, err
	}
	if len(rest) == 0 || rest[0] == "" {
		return nil, ErrNoImage
	}
	return &RunArgs{Flags: flags, Image: rest[0], Cmd: rest[1:]}, nil
}

// Values returns every value given for the option name (canonical), in order.
func (r *RunArgs) Values(name string) []string {
	return values(r.Flags, name)
}

// Value returns the last value given for the option name (canonical): docker
// keeps the last of a repeated single-valued option.
func (r *RunArgs) Value(name string) (string, bool) {
	return lastValue(r.Flags, name)
}

// Bool reports a boolean option's last value, or def when it is not given.
func (r *RunArgs) Bool(name string, def bool) bool {
	v, ok := r.Value(name)
	if !ok {
		return def
	}
	return v == "true"
}

// Names returns the canonical names of the options given, sorted and without
// repeats: a run's flag signature.
func (r *RunArgs) Names() []string {
	seen := map[string]bool{}
	var names []string
	for _, f := range r.Flags {
		if !seen[f.Name] {
			seen[f.Name] = true
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

// unsupportedFlagPrefix starts the error a create fails with when the
// devcontainer CLI emitted a docker run option the shim does not know.
const unsupportedFlagPrefix = "kubernetes:unsupported:flag:"

// UnsupportedFlag is the error code for a docker run option fleet cannot
// carry onto a cluster: "kubernetes:unsupported:flag:<flag>".
func UnsupportedFlag(flag string) string {
	return unsupportedFlagPrefix + strings.TrimLeft(flag, "-")
}

// runParseError explains a run argv ParseRun refused, in the form a create
// failure carries.
func runParseError(err error) error {
	var unknown *UnknownFlagError
	if errors.As(err, &unknown) {
		return fmt.Errorf("%s: the devcontainer CLI passed docker run %s, which fleet does not know how to run on Kubernetes", UnsupportedFlag(unknown.Flag), unknown.Flag)
	}
	return fmt.Errorf("parsing the devcontainer CLI's docker run: %w", err)
}
