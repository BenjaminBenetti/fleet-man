package fleet

// AllowsPrivileged reports whether instance pods on this connection may run
// privileged, with AllowPrivileged's auto resolved: an unset toggle allows
// them only on a local cluster (minikube, kind, k3d, Docker Desktop), where
// the node is the user's own machine. Elsewhere a privileged pod makes the
// node the isolation boundary, so it takes an explicit "Allow privileged
// pods" on the connection.
func (k *KubeCluster) AllowsPrivileged(localCluster bool) bool {
	return boolOr(k.AllowPrivileged, localCluster)
}

// RequiresDebugCapabilities reports whether the debug grants a devcontainer
// asks for (SYS_PTRACE, unconfined seccomp or AppArmor) must be granted, so
// a create fails when admission denies them, instead of the default (auto),
// which drops them with a warning.
func (k *KubeCluster) RequiresDebugCapabilities() bool {
	return k.DebugCapabilities == KubeDebugCapabilitiesRequire
}

// MirrorsHostGateway reports whether pods reach the daemon host's ports as
// host-gateway (host.docker.internal) when their devcontainer declares it;
// the default (auto) does.
func (k *KubeCluster) MirrorsHostGateway() bool {
	return k.HostGateway != KubeHostGatewayOff
}

// HostGatewayRemoteWarning is what a client of a remote daemon is told of a
// connection that mirrors the daemon host's ports (MirrorsHostGateway): the
// host its pods reach is the daemon's, not the client's machine.
const HostGatewayRemoteWarning = "fleetd runs on another host: pods that declare host-gateway reach every TCP port listening there, loopback-only ones too"

// HostGatewayWarning is the warning for a connection whose host_gateway is
// hostGateway, seen from a client whose daemon is remote or not: "" unless
// the daemon is remote and the mirror is on.
func HostGatewayWarning(hostGateway string, remote bool) string {
	if !remote || KubeHostGateway(hostGateway) == KubeHostGatewayOff {
		return ""
	}
	return HostGatewayRemoteWarning
}
