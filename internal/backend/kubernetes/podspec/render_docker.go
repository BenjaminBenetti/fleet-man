//go:build !nok8s

package podspec

import (
	"path"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The docker engine's sidecar (PodSpec.DooD; see docker.go): a native
// sidecar, an init container with restartPolicy Always, which the kubelet
// starts before the dev container and keeps running beside it. Its startup
// probe holds the dev container back until the engine answers, so the
// devcontainer's entrypoint (the docker-outside-of-docker feature's socket
// proxy) finds a socket.
//
// It runs docker's own dind image, but not that image's entrypoint, which
// would also listen on TCP 2375 without TLS (root on the node for anyone on
// the pod network) and nest cgroups at the root of the node's hierarchy (a
// privileged container shares the node's cgroup namespace on containerd and
// CRI-O). Its script does what the image's dind wrapper does, inside its own
// container cgroup, then runs dockerd on the shared socket with the shared
// daemon.json and the pod network's MTU, and sends the engine SIGHUP
// whenever that daemon.json changes: the dev container writes it (fleet's
// image cache step) but cannot signal another container's processes.

// dockerSidecarScript is the sidecar's script; its arguments are dockerd
// options of the connection's (the address pools). It is fleet's own text:
// it never holds "$(" or "$$", which the kubelet expands in a command
// (TestDockerSidecarScriptPassesKubeletExpansion).
const dockerSidecarScript = `set -eu
dir=` + DockerDir + `
sock="$dir/docker.sock"
cfg="$dir/daemon.json"
[ -s "$cfg" ] || echo '{}' > "$cfg"
rm -f "$sock"
find /run /var/run -iname 'docker*.pid' -delete 2>/dev/null || :
find /run /var/run -iname 'container*.pid' -delete 2>/dev/null || :
export container=docker
if [ -d /sys/kernel/security ] && ! mountpoint -q /sys/kernel/security; then
	mount -t securityfs none /sys/kernel/security 2>/dev/null || :
fi
mount --make-rshared / 2>/dev/null || :
parent=
cg=` + "`" + `sed -n 's/^0:://p' /proc/self/cgroup` + "`" + `
c="/sys/fs/cgroup${cg%/}"
if [ -n "$cg" ] && [ -w "$c/cgroup.subtree_control" ]; then
	mkdir -p "$c/init" "$c/docker"
	i=0
	until { xargs -rn1 < "$c/cgroup.procs" > "$c/init/cgroup.procs" || :; sed -e 's/ / +/g' -e 's/^/+/' < "$c/cgroup.controllers" > "$c/cgroup.subtree_control"; } 2>/dev/null; do
		i=` + "`" + `expr "$i" + 1` + "`" + `
		[ "$i" -lt 100 ] || break
		sleep 0.1
	done
	if [ "$i" -lt 100 ]; then
		parent="--cgroup-parent=${cg%/}/docker"
	else
		echo "fleet-docker: the engine's containers get no cgroup controllers: processes kept arriving in $c" >&2
	fi
fi
if ! iptables -nL >/dev/null 2>&1 && [ -x /usr/local/sbin/.iptables-legacy/iptables ] && /usr/local/sbin/.iptables-legacy/iptables -nL >/dev/null 2>&1; then
	PATH="/usr/local/sbin/.iptables-legacy:$PATH"
	export PATH
fi
iface=` + "`" + `awk '$2 == "00000000" && $8 == "00000000" { print $1; exit }' /proc/net/route 2>/dev/null || :` + "`" + `
mtu=` + "`" + `cat "/sys/class/net/${iface:-eth0}/mtu" 2>/dev/null || :` + "`" + `
dockerd --host="unix://$sock" --config-file="$cfg" ${mtu:+"--mtu=$mtu"} $parent "$@" &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null || :' TERM INT
sum=` + "`" + `cksum < "$cfg" 2>/dev/null || :` + "`" + `
while kill -0 "$pid" 2>/dev/null; do
	sleep 2 &
	wait "$!" || :
	now=` + "`" + `cksum < "$cfg" 2>/dev/null || :` + "`" + `
	if [ "$now" != "$sum" ]; then
		sum="$now"
		echo "fleet-docker: $cfg changed; reloading the engine" >&2
		kill -HUP "$pid" 2>/dev/null || :
	fi
done
wait "$pid"
`

// DockerSidecarCommand is the sidecar's command for dockerd options args:
// docker's docker-init (tini, which reaps the engine's orphans as PID 1)
// running the script.
func DockerSidecarCommand(args ...string) []string {
	return append([]string{"docker-init", "--", "/bin/sh", "-c", dockerSidecarScript, DockerSidecar}, args...)
}

// dockerReadyCommand is the sidecar's startup probe: the engine answers on
// its socket.
var dockerReadyCommand = []string{"docker", "--host", "unix://" + DockerSocket, "version", "--format", "{{.Server.Version}}"}

// The sidecar's startup probe allows its engine three minutes to come up (a
// first start on a fresh disk sets up its storage and networks).
const (
	dockerProbePeriod   = 1
	dockerProbeFailures = 180
	dockerProbeTimeout  = 5
)

// dockerDataMountPath is where the engine keeps its data, from the instance
// disk's vol/fleet-docker.
const dockerDataMountPath = "/var/lib/docker"

// dockerImage is the sidecar's image: the connection's service_images.dind,
// else DefaultDockerImage.
func (r *renderer) dockerImage() string {
	if img := strings.TrimSpace(r.cluster.ServiceImages.DinD); img != "" {
		return img
	}
	return DefaultDockerImage
}

// dockerSidecar is the docker engine's sidecar container.
func (r *renderer) dockerSidecar() corev1.Container {
	d := r.pod.DooD
	var args []string
	for _, p := range d.AddressPools {
		args = append(args, "--default-address-pool", "base="+p.Base+",size="+strconv.Itoa(p.Size))
	}
	// Root, as docker's engine runs.
	sc := &corev1.SecurityContext{RunAsUser: ptr(int64(0)), RunAsGroup: ptr(int64(0))}
	if d.Privileged {
		sc.Privileged = ptr(true)
	}
	return corev1.Container{
		Name:            DockerSidecar,
		Image:           r.dockerImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         DockerSidecarCommand(args...),
		RestartPolicy:   ptr(corev1.ContainerRestartPolicyAlways),
		VolumeMounts:    r.dockerSidecarMounts(),
		SecurityContext: sc,
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: append([]string(nil), dockerReadyCommand...)}},
			PeriodSeconds:    dockerProbePeriod,
			FailureThreshold: dockerProbeFailures,
			TimeoutSeconds:   dockerProbeTimeout,
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
}

// dockerSidecarMounts are the sidecar's mounts: the folder it shares with
// the dev container, its data on the instance disk, and the workspace, at
// the dev container's paths and at the daemon host's (what
// ${localWorkspaceFolder} names), so a sibling's bind of either finds it, as
// a bind of the daemon host's path does on docker.
func (r *renderer) dockerSidecarMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: volumeDocker, MountPath: DockerDir},
		{Name: volumeDisk, MountPath: dockerDataMountPath, SubPath: podseed.VolumeSubPath(DockerDataVolume)},
	}
	taken := map[string]bool{DockerDir: true, dockerDataMountPath: true}
	add := func(at, sub string, ro bool) {
		if taken[at] {
			return
		}
		taken[at] = true
		mounts = append(mounts, corev1.VolumeMount{Name: volumeDisk, MountPath: at, SubPath: sub, ReadOnly: ro})
	}
	var hostPaths []Mount
	for _, m := range r.pod.Mounts {
		if m.Kind != MountWorkspace {
			continue
		}
		sub := podseed.DirWorkspace
		if m.SubPath != "" {
			sub = path.Join(podseed.DirWorkspace, m.SubPath)
		}
		add(m.Target, sub, m.ReadOnly)
		hostPaths = append(hostPaths, m)
	}
	for _, m := range hostPaths {
		if at := path.Clean(m.Source); sidecarMayMount(at) {
			sub := podseed.DirWorkspace
			if m.SubPath != "" {
				sub = path.Join(podseed.DirWorkspace, m.SubPath)
			}
			add(at, sub, m.ReadOnly)
		}
	}
	return mounts
}

// sidecarSystemDirs are the sidecar's own dirs, where no workspace path may
// be mounted.
var sidecarSystemDirs = []string{"/bin", "/dev", "/etc", "/lib", "/lib64", "/opt/fleet", "/proc", "/run", "/sbin", "/sys", "/usr", "/var/lib/docker", "/var/run"}

// sidecarMayMount reports whether a daemon-host path can be mounted in the
// sidecar as it is: an absolute (POSIX) path outside its system dirs.
func sidecarMayMount(p string) bool {
	if !path.IsAbs(p) || p == "/" || strings.ContainsAny(p, "\\:") {
		return false
	}
	for _, d := range sidecarSystemDirs {
		if underTarget(p, d) {
			return false
		}
	}
	return true
}
