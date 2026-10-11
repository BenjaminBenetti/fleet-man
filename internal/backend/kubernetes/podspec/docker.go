package podspec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

// The docker engines an instance pod can have.
//
// Docker-in-docker (DinD): a privileged dev container (or one under a
// sysbox runtime class) may run its own dockerd, as the docker-in-docker
// feature does. pod-init writes the engine's daemon.json before the start
// script starts it (pid1.json docker): the pod network's MTU, so a nested
// network never fragments under an overlay CNI; the connection's
// dind_address_pool, which keeps nested networks off the cluster's own
// ranges; and, on a privileged pod, a cgroup parent inside fleet-user, so the
// engine's containers count against the pod and freeze with the user's
// processes. The feature's /var/lib/docker volume (named with the
// ${devcontainerId}) lives on the instance disk, vol/<name>.
//
// Docker-outside-of-docker (DooD): a run that binds the docker engine's
// socket would reach the daemon host's engine on docker. A pod never gets
// that (a relay of the laptop's socket would be root on the laptop): it gets
// an engine of its own, the fleet-docker native sidecar, privileged (or
// under sysbox), with its data on vol/fleet-docker, its socket and
// daemon.json in a folder it shares with the dev container, and each bind's
// target a link to that socket. Its containers are pod-local siblings,
// which the create says once.

// finishDocker decides the pod's docker engines, once the privilege policy
// has run.
func (t *translation) finishDocker() {
	underSysbox := t.sysbox || strings.Contains(t.spec.RuntimeClassName, "sysbox")
	dood := len(t.dood) > 0 && (t.doodPrivileged || t.sysbox)
	if !t.spec.Security.Privileged && !underSysbox && !dood {
		// No engine: the connection's pools do not concern the pod.
		return
	}
	pools, err := fleet.ParseDockerAddressPools(t.cluster.DinDAddressPool)
	if err != nil {
		t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("%s has dind_address_pool set to %q, which does not read: %v", t.clusterName(), t.cluster.DinDAddressPool, err)})
		return
	}
	engines := false
	if t.spec.Security.Privileged || underSysbox {
		// A dev container that can run dockerd; pod-init configures one
		// only when the image has it.
		t.spec.PID1.Docker = &podinit.Docker{AddressPools: podinitPools(pools)}
		engines = true
	}
	if dood {
		if t.spec.HostNetwork {
			// On docker such a run reaches the host's own engine; a new
			// one in the node's network namespace would rewrite the
			// node's firewall and bridges for every pod on it.
			t.refuse(&Refusal{Code: CodeDooDHostNetwork, Reason: fmt.Sprintf(
				"%s binds the docker engine's socket, and --network host puts the pod on the node's network; on a cluster that engine is the pod's own, which there would take over the node's iptables and bridges. Drop --network host, or the docker.sock bind",
				joinNames(t.dood))})
			return
		}
		if minor, ok := kubeMinor(t.in.Facts.ServerVersion); ok && minor < 29 {
			t.refuse(&Refusal{Code: CodeDooDNeedsSidecars, Reason: fmt.Sprintf(
				"%s binds the docker engine's socket; on a cluster that engine is the pod's own, a native sidecar, which needs Kubernetes 1.29 or later, and %s runs %s",
				joinNames(t.dood), t.clusterName(), t.in.Facts.ServerVersion)})
			return
		}
		t.spec.DooD = &DooD{Privileged: t.doodPrivileged, AddressPools: pools}
		var targets []string
		for _, m := range t.spec.Mounts {
			if m.Kind == MountDockerSocket {
				t.spec.PID1.Links = append(t.spec.PID1.Links, podinit.Link{Path: m.Target, Target: DockerSocket})
				targets = append(targets, m.Target)
			}
		}
		t.warn("%s: the docker engine is the pod's own (the %s sidecar), not the daemon host's: the containers it runs are pod-local siblings, whose published ports are on the instance's localhost and whose binds see the workspace and nothing else of the instance, and its images live on the instance disk",
			joinNames(targets), DockerSidecar)
		engines = true
	}
	if engines && t.in.Facts.DiskFileShare != "" {
		t.warn("docker: the engine keeps its data on the instance disk, whose storage class %s is a network file share, where docker's overlay storage does not run (expect a slow fallback, or an engine that does not start); use a block storage class for instance disks", t.in.Facts.DiskFileShare)
	}
}

func podinitPools(pools []fleet.DockerAddressPool) []podinit.AddressPool {
	var out []podinit.AddressPool
	for _, p := range pools {
		out = append(out, podinit.AddressPool{Base: p.Base, Size: p.Size})
	}
	return out
}

// kubeMinor is the minor version of a Kubernetes 1.x server version
// ("v1.29.4-gke.100" is 29); ok is false for one it cannot read.
func kubeMinor(v string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "1.")
	if !ok {
		return 0, false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[:end])
	return n, err == nil
}
