package podspec

import (
	"encoding/json"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// The option table and its golden corpus (testdata/options.golden.json):
// one or more cases per option, each a base run plus the case's options,
// recording what the case changes in the pod, its warnings and its error.

// The table has exactly the options kubeshim parses, once each, sorted, and
// every row says why.
func TestOptionTableCoversEveryRunOption(t *testing.T) {
	names := kubeshim.RunFlagNames()
	var table []string
	for _, o := range Options() {
		table = append(table, o.Name)
		if o.Reason == "" {
			t.Errorf("--%s has no reason", o.Name)
		}
		switch o.Outcome {
		case Map, Delta, Ignore, Refuse:
		default:
			t.Errorf("--%s has outcome %q", o.Name, o.Outcome)
		}
		if o.apply == nil && o.Outcome == Map && !readFromContainer[o.Name] {
			t.Errorf("--%s maps but has no apply and is not read from the merged container", o.Name)
		}
	}
	if !sort.StringsAreSorted(table) {
		t.Error("the table is not sorted by option name")
	}
	if strings.Join(table, " ") != strings.Join(names, " ") {
		t.Errorf("the table and kubeshim.RunFlagNames differ:\nmissing from the table: %v\nnot a docker run option: %v", minus(names, table), minus(table, names))
	}
}

// readFromContainer are the Map options Translate reads from the merged
// container (kubeshim.ContainerFor) or the capture, not through the table.
var readFromContainer = map[string]bool{
	"entrypoint": true, "env": true, "env-file": true, "label": true, "label-file": true, "user": true, "workdir": true,
}

func minus(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// The base of every option case: the run the devcontainer CLI emits for a
// plain image devcontainer, on a managed (non-local) connection.
const (
	baseImage = "mcr.microsoft.com/devcontainers/base:debian-12"
	baseWS    = "/home/dev/src/app"
)

var baseArgs = []string{
	"--sig-proxy=false", "-a", "STDOUT", "-a", "STDERR",
	"--mount", "type=bind,source=" + baseWS + ",target=/workspaces/app",
	"-l", "devcontainer.local_folder=" + baseWS,
	"-l", "devcontainer.config_file=" + baseWS + "/.devcontainer/devcontainer.json",
	"--entrypoint", "/bin/sh",
}

var baseCmd = []string{"-c", "echo Container started\ntrap \"exit 0\" 15\n\nexec \"$@\"\nwhile sleep 1 & wait $!; do :; done", "-"}

// baseHost is the daemon host the option cases bind from.
var baseHost = fakeFS{
	"/home/dev":                   fs.ModeDir | 0o755,
	baseWS:                        fs.ModeDir | 0o755,
	baseWS + "/sub":               fs.ModeDir | 0o755,
	"/home/dev/host":              fs.ModeDir | 0o755,
	"/home/dev/host/agent.sock":   fs.ModeSocket | 0o600,
	"/home/dev/host/S.gpg-agent":  fs.ModeSocket | 0o600,
	"/home/dev/host/aws":          fs.ModeDir | 0o700,
	"/home/dev/host/aws/config":   0o600,
	"/home/dev/host/gitconfig":    0o644,
	"/home/dev/host/x11":          fs.ModeDir | 0o1777,
	"/home/dev/host/x11/X0":       fs.ModeSocket | 0o777,
	"/home/dev/host/fifo":         fs.ModeNamedPipe | 0o600,
	"/dev/kvm":                    fs.ModeDevice | fs.ModeCharDevice | 0o660,
	"/var/run/docker.sock":        fs.ModeSocket | 0o660,
	"/home/dev/host/empty-dir":    fs.ModeDir | 0o755,
	"/home/dev/host/aws/sessions": fs.ModeDir | 0o700,
}

// baseInput is the input of a case giving extra options and its env.
func baseInput(extra []string, env []kubeshim.EnvVar, labels map[string]string) Input {
	args := append(append(append([]string{}, baseArgs...), extra...), baseImage)
	args = append(args, baseCmd...)
	l := map[string]string{
		"devcontainer.local_folder": baseWS,
		"devcontainer.config_file":  baseWS + "/.devcontainer/devcontainer.json",
	}
	for k, v := range labels {
		l[k] = v
	}
	var img kubeshim.ImageConfig
	img.ID = "sha256:23fa69fed758b7927c60061317d73baf7d66b9fca5c344e80bce3a940b229af0"
	img.Os, img.Architecture = "linux", "amd64"
	img.Config.User = "root"
	img.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "IMAGE_ONLY=from-image"}
	img.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}, "5353/udp": {}}
	return Input{
		Capture:          &kubeshim.Capture{Format: kubeshim.CaptureFormat, Verb: "run", Args: args, Env: env, Labels: l},
		Image:            img,
		Merged:           json.RawMessage(`{"remoteUser":"vscode"}`),
		WorkspaceDir:     baseWS,
		WorkspaceFolder:  "/workspaces/app",
		Cluster:          &fleet.KubeCluster{ID: "c-prod", DisplayName: "prod-eu", Namespace: "team-dev"},
		Facts:            ClusterFacts{RuntimeClasses: []string{"gvisor"}, Nodes: []Node{{Arch: "amd64", AllocatableCPU: "8", AllocatableMemory: "32Gi"}}},
		Host:             baseHost,
		HostAgentSockets: []string{"/home/dev/host/agent.sock"},
	}
}

// local makes a case's connection a local cluster.
func local(in *Input) { in.Facts.Local = true }

// optionCase is one case of the corpus. Its name starts with the option it
// is about.
type optionCase struct {
	name   string
	args   []string
	env    []kubeshim.EnvVar
	labels map[string]string
	in     func(*Input)
}

// devIDOfBase is the base run's ${devcontainerId}.
var devIDOfBase = devcontainerID(map[string]string{
	"devcontainer.local_folder": baseWS,
	"devcontainer.config_file":  baseWS + "/.devcontainer/devcontainer.json",
})

var optionCases = []optionCase{
	{name: "add-host", args: []string{"--add-host=db:10.0.0.5", "--add-host", "cache=10.0.0.5", "--add-host=v6=[::1]"}},
	{name: "add-host/host-gateway", args: []string{"--add-host=host.docker.internal:host-gateway"}},
	{name: "add-host/host-gateway-off", args: []string{"--add-host=host.docker.internal:host-gateway"},
		in: func(in *Input) { in.Cluster.HostGateway = fleet.KubeHostGatewayOff }},
	{name: "add-host/host-gateway-other-name", args: []string{"--add-host=gw:host-gateway"}},
	{name: "add-host/host-gateway-own-address", args: []string{"--add-host=gw:host-gateway", "--add-host=host.docker.internal:10.0.0.9"}},
	{name: "add-host/invalid", args: []string{"--add-host=db:not-an-ip"}},
	{name: "annotation", args: []string{"--annotation", "com.example.key=the-value"}},
	{name: "attach", args: []string{"-a", "STDIN"}},
	{name: "blkio-weight", args: []string{"--blkio-weight", "300"}},
	{name: "blkio-weight-device", args: []string{"--blkio-weight-device", "/dev/sda:200"}},
	{name: "cap-add", args: []string{"--cap-add", "NET_ADMIN", "--cap-add", "cap_sys_admin"}},
	{name: "cap-add/sys-ptrace", args: []string{"--cap-add", "CAP_SYS_PTRACE"}},
	{name: "cap-add/sys-ptrace-dropped", args: []string{"--cap-add", "SYS_PTRACE"}, in: func(in *Input) { in.DropDebugGrants = true }},
	{name: "cap-add/all", args: []string{"--cap-add", "all"}},
	{name: "cap-add/invalid", args: []string{"--cap-add", "not a cap"}},
	{name: "cap-drop", args: []string{"--cap-drop", "NET_RAW", "--cap-drop", "MKNOD"}},
	{name: "cap-drop/added-back", args: []string{"--cap-drop", "ALL", "--cap-drop", "chown", "--cap-add", "CHOWN"}},
	{name: "cgroup-parent", args: []string{"--cgroup-parent", "/custom"}},
	{name: "cgroupns", args: []string{"--cgroupns", "private"}},
	{name: "cgroupns/host", args: []string{"--cgroupns=host"}},
	{name: "cidfile", args: []string{"--cidfile", "/tmp/cid"}},
	{name: "cpu-count", args: []string{"--cpu-count", "2"}},
	{name: "cpu-percent", args: []string{"--cpu-percent", "50"}},
	{name: "cpu-period", args: []string{"--cpu-period", "50000"}},
	{name: "cpu-quota", args: []string{"--cpu-quota", "150000"}},
	{name: "cpu-quota/with-period", args: []string{"--cpu-quota", "150000", "--cpu-period", "50000"}},
	{name: "cpu-quota/unlimited", args: []string{"--cpu-quota", "-1"}},
	{name: "cpu-rt-period", args: []string{"--cpu-rt-period", "1000000"}},
	{name: "cpu-rt-runtime", args: []string{"--cpu-rt-runtime", "950000"}},
	{name: "cpu-shares", args: []string{"-c", "512"}},
	{name: "cpus", args: []string{"--cpus", "1.5"}},
	{name: "cpus/below-request", args: []string{"--cpus", "0.25"}},
	{name: "cpus/with-quota", args: []string{"--cpus", "2", "--cpu-quota", "100000"}},
	{name: "cpuset-cpus", args: []string{"--cpuset-cpus", "0-3"}},
	{name: "cpuset-mems", args: []string{"--cpuset-mems", "0"}},
	{name: "detach", args: []string{"-d"}},
	{name: "detach-keys", args: []string{"--detach-keys", "ctrl-x"}},
	{name: "device", args: []string{"--device", "/dev/kvm"}},
	{name: "device/local", args: []string{"--device", "/dev/kvm:/dev/kvm:rw"}, in: local},
	{name: "device/invalid", args: []string{"--device", "kvm"}, in: local},
	{name: "device-cgroup-rule", args: []string{"--device-cgroup-rule", "c 81:* rmw"}},
	{name: "device-cgroup-rule/privileged", args: []string{"--privileged", "--device-cgroup-rule", "c 81:* rmw"}, in: local},
	{name: "device-read-bps", args: []string{"--device-read-bps", "/dev/sda:1mb"}},
	{name: "device-read-iops", args: []string{"--device-read-iops", "/dev/sda:1000"}},
	{name: "device-write-bps", args: []string{"--device-write-bps", "/dev/sda:1mb"}},
	{name: "device-write-iops", args: []string{"--device-write-iops", "/dev/sda:1000"}},
	{name: "disable-content-trust", args: []string{"--disable-content-trust"}},
	{name: "dns", args: []string{"--dns", "1.1.1.1", "--dns=8.8.8.8"}},
	{name: "dns/invalid", args: []string{"--dns", "resolver.example"}},
	{name: "dns-option", args: []string{"--dns-option", "ndots:2", "--dns-opt", "edns0"}},
	{name: "dns-search", args: []string{"--dns-search", "corp.example", "--dns-search", "."}},
	{name: "domainname", args: []string{"--domainname", "corp.example"}},
	{name: "domainname/sys-admin", args: []string{"--domainname", "corp.example", "--cap-add", "SYS_ADMIN"}},
	{name: "entrypoint", args: []string{"--entrypoint", "/usr/local/bin/start.sh"}},
	{name: "entrypoint/reset", args: []string{"--entrypoint="}},
	{name: "env", args: []string{"-e", "API_TOKEN=s3cr3t-$(HOME)-value", "--env=PLAIN=1"},
		env: []kubeshim.EnvVar{{Name: "API_TOKEN", Value: "s3cr3t-$(HOME)-value", Source: "flag"}, {Name: "PLAIN", Value: "1", Source: "flag"}}},
	{name: "env/unset", args: []string{"-e", "IMAGE_ONLY", "-e", "NOT_ANYWHERE"},
		env: []kubeshim.EnvVar{{Name: "IMAGE_ONLY", Unset: true, Source: "host"}, {Name: "NOT_ANYWHERE", Unset: true, Source: "host"}}},
	{name: "env/host-value", args: []string{"-e", "FROM_HOST"}, env: []kubeshim.EnvVar{{Name: "FROM_HOST", Value: "host-side", Source: "host"}}},
	{name: "env/names", args: []string{"-e", "my.var-1=a", "-e", "with space=b", "-e", "x-key=c"},
		env: []kubeshim.EnvVar{{Name: "my.var-1", Value: "a", Source: "flag"}, {Name: "with space", Value: "b", Source: "flag"}, {Name: "x-key", Value: "c", Source: "flag"}}},
	{name: "env-file", args: []string{"--env-file", baseWS + "/.env"},
		env: []kubeshim.EnvVar{{Name: "FROM_FILE", Value: "file-value", Source: "file:" + baseWS + "/.env"}}},
	{name: "expose", args: []string{"--expose", "9000"}},
	{name: "expose/publish-all", args: []string{"--expose", "9000-9001", "--expose", "53/udp", "-P"}},
	{name: "gpus", args: []string{"--gpus", "all"}},
	{name: "gpus/all", args: []string{"--gpus", "all"}, in: func(in *Input) { in.Facts.GPUResource = "nvidia.com/gpu" }},
	{name: "gpus/count", args: []string{"--gpus", `count=2,"capabilities=compute,utility"`}, in: func(in *Input) { in.Facts.GPUResource = "nvidia.com/gpu" }},
	{name: "gpus/quoted-count", args: []string{"--gpus", `"count=2,capabilities=compute"`}, in: func(in *Input) { in.Facts.GPUResource = "nvidia.com/gpu" }},
	{name: "gpus/devices", args: []string{"--gpus", `"device=0,1"`}, in: func(in *Input) { in.Facts.GPUResource = "nvidia.com/gpu" }},
	{name: "group-add", args: []string{"--group-add", "999", "--group-add", "999"}},
	{name: "group-add/name", args: []string{"--group-add", "docker"}, in: func(in *Input) {
		in.LookupGroup = func(name string) (int64, bool) { return 998, name == "docker" }
	}},
	{name: "group-add/unknown", args: []string{"--group-add", "nosuchgroup"}},
	{name: "health-cmd", args: []string{"--health-cmd", "curl -f localhost", "--health-interval", "5s", "--health-retries", "3",
		"--health-start-interval", "1s", "--health-start-period", "10s", "--health-timeout", "2s"}},
	{name: "health-interval", args: []string{"--health-interval", "30s"}},
	{name: "health-retries", args: []string{"--health-retries", "5"}},
	{name: "health-start-interval", args: []string{"--health-start-interval", "2s"}},
	{name: "health-start-period", args: []string{"--health-start-period", "20s"}},
	{name: "health-timeout", args: []string{"--health-timeout", "3s"}},
	{name: "help", args: []string{"--help"}},
	{name: "hostname", args: []string{"--hostname", "devbox"}},
	{name: "hostname/privileged", args: []string{"-h", "devbox", "--privileged"}, in: local},
	{name: "hostname/host-network", args: []string{"-h", "devbox", "--domainname", "corp.example", "--privileged", "--network", "host"}, in: local},
	{name: "init", args: []string{"--init"}},
	{name: "interactive", args: []string{"-i"}},
	{name: "io-maxbandwidth", args: []string{"--io-maxbandwidth", "10mb"}},
	{name: "io-maxiops", args: []string{"--io-maxiops", "1000"}},
	{name: "ip", args: []string{"--ip", "172.20.0.5"}},
	{name: "ip6", args: []string{"--ip6", "fd00::5"}},
	{name: "ipc", args: []string{"--ipc", "shareable"}},
	{name: "ipc/host", args: []string{"--ipc", "host"}},
	{name: "ipc/host-local", args: []string{"--ipc", "host"}, in: local},
	{name: "ipc/none", args: []string{"--ipc", "none"}},
	{name: "ipc/container", args: []string{"--ipc", "container:other"}},
	{name: "isolation", args: []string{"--isolation", "default"}},
	{name: "isolation/hyperv", args: []string{"--isolation", "hyperv"}},
	{name: "kernel-memory", args: []string{"--kernel-memory", "1g"}},
	{name: "label", args: []string{"-l", "com.example.team=dev"}, labels: map[string]string{"com.example.team": "dev"}},
	{name: "label-file", args: []string{"--label-file", baseWS + "/labels"}, labels: map[string]string{"from.file": "1"}},
	{name: "link", args: []string{"--link", "db:database"}},
	{name: "link-local-ip", args: []string{"--link-local-ip", "169.254.1.5"}},
	{name: "log-driver", args: []string{"--log-driver", "none"}},
	{name: "log-opt", args: []string{"--log-opt", "max-size=10m", "--log-opt", "max-file=3"}},
	{name: "log-opt/credentials", args: []string{"--log-driver", "splunk", "--log-opt", "splunk-token=hunter22", "--log-opt", "splunk-url=https://splunk.example:8088"}},
	{name: "mac-address", args: []string{"--mac-address", "02:42:ac:11:00:02"}},
	{name: "memory", args: []string{"-m", "4g"}},
	{name: "memory/below-request", args: []string{"--memory", "1g"}},
	{name: "memory/invalid", args: []string{"--memory", "lots"}},
	{name: "memory/limitrange", args: nil, in: func(in *Input) { in.Facts.LimitRangeMemoryLimit = "1Gi" }},
	{name: "memory/limitrange-high", args: nil, in: func(in *Input) { in.Facts.LimitRangeMemoryLimit = "16Gi" }},
	{name: "memory/limitrange-with-limit", args: []string{"-m", "512m"}, in: func(in *Input) { in.Facts.LimitRangeMemoryLimit = "1Gi" }},
	{name: "memory/autopilot", args: nil, in: func(in *Input) { in.Facts.Autopilot = true }},
	{name: "memory/autopilot-with-limit", args: []string{"-m", "6g", "--cpus", "2"}, in: func(in *Input) { in.Facts.Autopilot = true }},
	{name: "memory-reservation", args: []string{"--memory-reservation", "3g"}},
	{name: "memory-reservation/above-limit", args: []string{"--memory-reservation", "3g", "-m", "2g"}},
	{name: "memory-swap", args: []string{"--memory-swap", "8g"}},
	{name: "memory-swappiness", args: []string{"--memory-swappiness", "0"}},
	{name: "mount/volume-named", args: []string{"--mount", "type=volume,source=cache,target=/cache"}},
	{name: "mount/volume-devcontainer-id", args: []string{"--mount", "type=volume,src=dind-var-lib-docker-" + devIDOfBase + ",dst=/var/lib/docker"}},
	{name: "mount/volume-anonymous", args: []string{"--mount", "type=volume,target=/data"}},
	{name: "mount/volume-nocopy-subpath", args: []string{"--mount", "type=volume,source=shared,target=/shared,volume-nocopy,readonly", "--mount", "type=volume,source=shared,target=/part,volume-subpath=a/b"}},
	{name: "mount/volume-subpath-escape", args: []string{"--mount", "type=volume,source=shared,target=/part,volume-subpath=../x"}},
	{name: "mount/tmpfs", args: []string{"--mount", "type=tmpfs,target=/scratch,tmpfs-size=64m,tmpfs-mode=1770"}},
	{name: "mount/bind-workspace-folder", args: []string{"--mount", `type=bind,"source=` + baseWS + `/sub",target=/sub,bind-propagation=rslave,consistency=cached`}},
	{name: "mount/bind-agent", args: []string{"--mount", "type=bind,source=/home/dev/host/agent.sock,target=/run/agent.sock"}},
	{name: "mount/bind-socket", args: []string{"--mount", "type=bind,source=/home/dev/host/S.gpg-agent,target=/run/gpg.sock"}},
	{name: "mount/bind-socket-dir", args: []string{"--mount", "type=bind,source=/home/dev/host/x11,target=/tmp/.X11-unix"}},
	{name: "mount/bind-dir-file", args: []string{"--mount", "type=bind,source=/home/dev/host/aws,target=/home/vscode/.aws", "--mount", "type=bind,source=/home/dev/host/gitconfig,target=/home/vscode/.gitconfig,ro=true"}},
	{name: "mount/bind-empty-dir", args: []string{"--mount", "type=bind,source=/home/dev/host/empty-dir,target=/empty"}},
	{name: "mount/bind-missing", args: []string{"--mount", "type=bind,source=/home/dev/host/nope,target=/nope"}},
	{name: "mount/bind-device", args: []string{"--mount", "type=bind,source=/dev/kvm,target=/dev/kvm"}},
	{name: "mount/bind-fifo", args: []string{"--mount", "type=bind,source=/home/dev/host/fifo,target=/fifo"}},
	{name: "mount/docker-socket", args: []string{"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker-host.sock"}},
	{name: "mount/docker-socket-local", args: []string{"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker-host.sock"}, in: local},
	{name: "mount/reserved", args: []string{"--mount", "type=volume,target=/opt/fleet/bin", "--mount", "type=tmpfs,target=/fleet-mounts/control"}},
	{name: "mount/duplicate", args: []string{"--mount", "type=volume,target=/data", "--mount", "type=tmpfs,target=/data/"}},
	{name: "mount/root", args: []string{"--mount", "type=volume,target=/"}},
	{name: "mount/fleet-wins", args: []string{"--mount", "type=volume,source=mine,target=/home/vscode/.claude"}, in: func(in *Input) {
		in.FleetMounts = []backend.Mount{
			{LocalPath: "/home/dev/.fleet/workspaces/f/.claude", ContainerPath: "/home/vscode/.claude", Scope: backend.MountScopeFleetShared, SubPath: ".claude"},
			{LocalPath: "/home/dev/.fleet/workspaces/f/files", ContainerPath: "/fleet-mounts/files", Scope: backend.MountScopeFleetShared, SubPath: "files", ReadOnly: true},
		}
	}},
	{name: "mount/fleet-host-scope", args: nil, in: func(in *Input) {
		in.FleetMounts = []backend.Mount{{LocalPath: "/srv/data", ContainerPath: "/data"}}
	}},
	{name: "mount/volume-opt", args: []string{"--mount", "type=volume,source=nfs,target=/nfs,volume-opt=type=nfs,volume-opt=device=:/export"}},
	{name: "mount/volume-driver", args: []string{"--mount", "type=volume,source=x,target=/x,volume-driver=rexray"}},
	{name: "mount/type-image", args: []string{"--mount", "type=image,source=alpine,target=/img"}},
	{name: "mount/relative-target", args: []string{"--mount", "type=volume,target=data"}},
	{name: "mount/unknown-option", args: []string{"--mount", "type=bind,source=/home/dev/host/aws,target=/aws,bogus=1"}},
	{name: "name", args: []string{"--name", "mycontainer"}},
	{name: "network", args: []string{"--network", "bridge"}},
	{name: "network/host", args: []string{"--network", "host"}},
	{name: "network/host-local", args: []string{"--net", "host"}, in: local},
	{name: "network/custom", args: []string{"--network", "my-net"}},
	{name: "network/none", args: []string{"--network", "none"}},
	{name: "network/container", args: []string{"--network", "container:other"}},
	{name: "network-alias", args: []string{"--network-alias", "api"}},
	{name: "no-healthcheck", args: []string{"--no-healthcheck"}},
	{name: "oom-kill-disable", args: []string{"--oom-kill-disable"}},
	{name: "oom-kill-disable/false", args: []string{"--oom-kill-disable=false"}},
	{name: "oom-score-adj", args: []string{"--oom-score-adj", "500"}},
	{name: "oom-score-adj/negative", args: []string{"--oom-score-adj", "-500"}},
	{name: "pid", args: []string{"--pid", "host"}},
	{name: "pid/host-local", args: []string{"--pid", "host"}, in: local},
	{name: "pid/container", args: []string{"--pid", "container:other"}},
	{name: "pids-limit", args: []string{"--pids-limit", "512"}},
	{name: "platform", args: []string{"--platform", "linux/amd64"}},
	{name: "platform/mismatch", args: []string{"--platform", "linux/arm64"}},
	{name: "platform/node-selector", args: nil, in: func(in *Input) {
		in.Cluster.NodeSelector = map[string]string{"pool": "dev", "kubernetes.io/arch": "arm64"}
	}},
	{name: "privileged", args: []string{"--privileged"}},
	{name: "privileged/local", args: []string{"--privileged"}, in: local},
	{name: "privileged/allowed", args: []string{"--privileged"}, in: func(in *Input) { yes := true; in.Cluster.AllowPrivileged = &yes }},
	{name: "privileged/refused-local", args: []string{"--privileged"}, in: func(in *Input) { no := false; in.Facts.Local = true; in.Cluster.AllowPrivileged = &no }},
	{name: "privileged/sysbox", args: []string{"--privileged"}, in: func(in *Input) { in.Facts.RuntimeClasses = []string{"gvisor", "sysbox-runc"} }},
	{name: "privileged/sysbox-runtime-class", args: []string{"--privileged"}, in: func(in *Input) { in.Cluster.RuntimeClass = "sysbox-runc" }},
	{name: "privileged/sysbox-host-network", args: []string{"--privileged", "--network", "host"}, in: func(in *Input) { in.Facts.RuntimeClasses = []string{"sysbox-runc"} }},
	{name: "privileged/false", args: []string{"--privileged=false"}},
	{name: "publish", args: []string{"-p", "8080:80", "-p", "127.0.0.1::9000"}},
	{name: "publish/address", args: []string{"-p", "0.0.0.0:8443:443", "-p", "[::1]:8081:81"}},
	{name: "publish/ephemeral-range", args: []string{"-p", "3000-3001", "-p", "4000-4001:5000-5001"}},
	{name: "publish/udp", args: []string{"-p", "5353:5353/udp"}},
	{name: "publish/host-range", args: []string{"-p", "8000-8010:80"}},
	{name: "publish/invalid", args: []string{"-p", "http"}},
	{name: "publish-all", args: []string{"-P", "-p", "8080:8080"}},
	{name: "pull", args: []string{"--pull", "always"}},
	{name: "quiet", args: []string{"-q"}},
	{name: "read-only", args: []string{"--read-only"}},
	{name: "restart", args: []string{"--restart", "unless-stopped"}},
	{name: "restart/no", args: []string{"--restart=no"}},
	{name: "restart/invalid", args: []string{"--restart", "sometimes"}},
	{name: "rm", args: []string{"--rm"}},
	{name: "runtime", args: []string{"--runtime", "gvisor"}},
	{name: "runtime/runc", args: []string{"--runtime", "runc"}},
	{name: "runtime/missing", args: []string{"--runtime", "kata"}},
	{name: "runtime/unknown-classes", args: []string{"--runtime", "kata"}, in: func(in *Input) { in.Facts.RuntimeClasses = nil }},
	{name: "runtime/conflict", args: []string{"--runtime", "gvisor"}, in: func(in *Input) { in.Cluster.RuntimeClass = "kata" }},
	{name: "runtime/cluster-class", args: nil, in: func(in *Input) { in.Cluster.RuntimeClass = "kata" }},
	{name: "security-opt", args: []string{"--security-opt", "no-new-privileges", "--security-opt", "apparmor=my-profile", "--security-opt", "label=type:container_t", "--security-opt", "label=user:system_u"}},
	{name: "security-opt/debug-grants", args: []string{"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor:unconfined"}},
	{name: "security-opt/debug-grants-dropped", args: []string{"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--cap-add", "SYS_PTRACE", "--cap-add", "NET_ADMIN"},
		in: func(in *Input) { in.DropDebugGrants = true }},
	{name: "security-opt/defaults", args: []string{"--security-opt", "seccomp=builtin", "--security-opt", "apparmor=docker-default", "--security-opt", "no-new-privileges=false"}},
	{name: "security-opt/label-disable", args: []string{"--security-opt", "label=disable"}},
	{name: "security-opt/label-level", args: []string{"--security-opt", "label=level:s0:c100,c200"}},
	{name: "security-opt/seccomp-file", args: []string{"--security-opt", "seccomp=/home/dev/seccomp.json"}},
	{name: "security-opt/systempaths", args: []string{"--security-opt", "systempaths=unconfined"}},
	{name: "security-opt/systempaths-privileged", args: []string{"--security-opt", "systempaths=unconfined", "--security-opt", "writable-cgroups=true", "--privileged"}, in: local},
	{name: "security-opt/unknown", args: []string{"--security-opt", "bogus=1", "--security-opt", "seccomp"}},
	{name: "shm-size", args: []string{"--shm-size", "1g"}},
	{name: "shm-size/invalid", args: []string{"--shm-size", "big"}},
	{name: "sig-proxy", args: []string{"--sig-proxy=true"}},
	{name: "stop-signal", args: []string{"--stop-signal", "USR1"}},
	{name: "stop-signal/number", args: []string{"--stop-signal", "10"}},
	{name: "stop-signal/realtime", args: []string{"--stop-signal", "SIGRTMIN+3"}},
	{name: "stop-signal/invalid", args: []string{"--stop-signal", "SIGNOPE"}},
	{name: "stop-signal/image", args: nil, in: func(in *Input) { in.Image.Config.StopSignal = "SIGQUIT" }},
	{name: "stop-timeout", args: []string{"--stop-timeout", "30"}},
	{name: "stop-timeout/unlimited", args: []string{"--stop-timeout", "-1"}},
	{name: "storage-opt", args: []string{"--storage-opt", "size=120G"}},
	{name: "sysctl", args: []string{"--sysctl", "net.ipv4.ip_unprivileged_port_start=0", "--sysctl", "net/ipv4/ping_group_range=0 2147483647", "--sysctl", "net.ipv4.ip_unprivileged_port_start=80"}},
	{name: "sysctl/unsafe", args: []string{"--sysctl", "net.core.somaxconn=1024"}},
	{name: "tmpfs", args: []string{"--tmpfs", "/run:size=64m,noexec,mode=755", "--tmpfs", "/cache"}},
	{name: "tmpfs/relative", args: []string{"--tmpfs", "run"}},
	{name: "tty", args: []string{"-t"}},
	{name: "ulimit", args: []string{"--ulimit", "nofile=4096:8192", "--ulimit", "nproc=512", "--ulimit", "nofile=2048:4096"}},
	{name: "ulimit/sys-resource", args: []string{"--ulimit", "memlock=unlimited:unlimited", "--cap-add", "SYS_RESOURCE"}},
	{name: "ulimit/invalid", args: []string{"--ulimit", "nofile=10:5", "--ulimit", "bogus=1"}},
	{name: "umask", args: []string{"--umask", "0027"}},
	{name: "umask/invalid", args: []string{"--umask", "999"}},
	{name: "use-api-socket", args: []string{"--use-api-socket"}},
	{name: "use-api-socket/local", args: []string{"--use-api-socket"}, in: local},
	{name: "user", args: []string{"-u", "vscode:vscode"}},
	{name: "userns", args: []string{"--userns", "host"}},
	{name: "userns/remap", args: []string{"--userns", "keep-id"}},
	{name: "uts", args: []string{"--uts", "host"}},
	{name: "volume", args: []string{"-v", "/home/dev/host/aws:/home/vscode/.aws:ro,z"}},
	{name: "volume/named", args: []string{"-v", "cache:/cache:nocopy"}},
	{name: "volume/anonymous", args: []string{"-v", "/data"}},
	{name: "volume/missing-source", args: []string{"-v", "/home/dev/host/new:/new"}},
	{name: "volume/relative", args: []string{"-v", "./data:/data"}},
	{name: "volume/invalid", args: []string{"-v", "/data:rw"}},
	{name: "volume-driver", args: []string{"--volume-driver", "local"}},
	{name: "volume-driver/plugin", args: []string{"--volume-driver", "rexray"}},
	{name: "volumes-from", args: []string{"--volumes-from", "other"}},
	{name: "workdir", args: []string{"-w", "/srv/app"}},
}

// optionResult is one case in the golden file.
type optionResult struct {
	Args []string `json:"args"`
	// Outcomes are the case's own options' outcomes.
	Outcomes map[string]Outcome `json:"outcomes"`
	// Changes are the pod fields the case changes, by JSON pointer.
	Changes  map[string]json.RawMessage `json:"changes,omitempty"`
	Warnings []string                   `json:"warnings,omitempty"`
	Error    string                     `json:"error,omitempty"`
}

func TestOptionGoldens(t *testing.T) {
	base, err := Translate(baseInput(nil, nil, nil))
	if err != nil {
		t.Fatalf("the base run does not translate: %v", err)
	}
	if len(base.Pod.Warnings) > 0 {
		t.Fatalf("the base run has warnings: %v", base.Pod.Warnings)
	}
	results := map[string]optionResult{}
	exercised := map[string]map[Outcome]bool{}
	for _, c := range optionCases {
		in := baseInput(c.args, c.env, c.labels)
		if c.in != nil {
			c.in(&in)
		}
		own, err := kubeshim.ParseRun(append(append([]string{}, c.args...), "image"))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if opt, _, _ := strings.Cut(c.name, "/"); len(c.args) > 0 && !containsString(own.Names(), opt) {
			t.Errorf("%s: the case does not give --%s", c.name, opt)
		}
		r := optionResult{Args: c.args, Outcomes: map[string]Outcome{}}
		res, err := Translate(in)
		if err != nil {
			r.Error = err.Error()
			// A failed translation has no outcomes; a refused option is
			// named by its code.
			for _, ref := range Refusals(err) {
				if name, ok := strings.CutPrefix(ref.Code, kubeshim.UnsupportedFlag("")); ok {
					r.Outcomes[name] = Refuse
				}
			}
		} else {
			for _, o := range res.Pod.Options {
				if containsString(own.Names(), o.Option) {
					r.Outcomes[o.Option] = o.Outcome
				}
			}
			r.Warnings = res.Pod.Warnings
			strip := func(p PodSpec) PodSpec {
				p.Options, p.Warnings = nil, nil
				return p
			}
			r.Changes = changes(t, strip(base.Pod), strip(res.Pod))
			checkNoEnvValues(t, c.name, res, in)
		}
		for name, o := range r.Outcomes {
			if exercised[name] == nil {
				exercised[name] = map[Outcome]bool{}
			}
			exercised[name][o] = true
		}
		if _, dup := results[c.name]; dup {
			t.Errorf("two cases are named %s", c.name)
		}
		results[c.name] = r
	}
	// Every option has a case, and its common-case outcome is exercised.
	for _, o := range Options() {
		if !exercised[o.Name][o.Outcome] {
			t.Errorf("no case gives --%s its table outcome %s (cases reached %v)", o.Name, o.Outcome, exercised[o.Name])
		}
	}
	checkGolden(t, "options.golden.json", mustIndent(t, results))
}

// checkNoEnvValues fails when an environment value the run sets shows in
// the pod model or in capture.json: values live only in the env Secret.
func checkNoEnvValues(t *testing.T, name string, res *Result, in Input) {
	t.Helper()
	pod := jsonString(res.Pod)
	capture := string(res.Secrets.Spec[CaptureFile])
	for _, e := range in.Capture.Env {
		if e.Unset || len(e.Value) < 3 {
			continue
		}
		if strings.Contains(pod, e.Value) {
			t.Errorf("%s: the value of %s shows in the pod model", name, e.Name)
		}
		if strings.Contains(capture, e.Value) {
			t.Errorf("%s: the value of %s shows in capture.json", name, e.Name)
		}
		if string(res.Secrets.Env[envKey(e.Name)]) != e.Value {
			t.Errorf("%s: the env Secret holds %q for %s", name, res.Secrets.Env[envKey(e.Name)], e.Name)
		}
	}
}
