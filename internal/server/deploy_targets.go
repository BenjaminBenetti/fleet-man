package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deploy_targets.go decides where a create can go. The daemon runs the CLIs a
// deploy target needs, so ListDeployTargets judges availability by THIS host's
// PATH, and a remote client gets the daemon's answer rather than its own
// machine's. resolveTarget turns a create request's target, which may be
// unspecified, into a concrete one from the remembered default, the same
// default ListDeployTargets reports; the TUI's New instance dialog writes it
// with CreateInstanceRequest.remember_default.

// daemonLookPath finds a binary on the daemon's PATH. A seam for tests.
var daemonLookPath = exec.LookPath

// daemonHostname names the daemon host in "on fleetd @ <host>". A seam for
// tests.
var daemonHostname = os.Hostname

// ListDeployTargets lists the targets a create in req.fleet can use, whether
// each is available on this host, the default an unspecified create gets, and
// this host's build tools. Served to remote clients as to local ones: it reads
// only what any client could already learn by creating an instance.
func (s *service) ListDeployTargets(_ context.Context, req *fleetgrpc.ListDeployTargetsRequest) (*fleetgrpc.ListDeployTargetsReply, error) {
	remote := ""
	if name := req.GetFleet(); name != "" {
		st, err := state.Load()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load state: %v", err)
		}
		if f, ok := st.Fleets[name]; ok {
			remote = f.Remote
		}
	}
	host, _ := daemonHostname()
	return &fleetgrpc.ListDeployTargetsReply{
		Targets:       protoconv.DeployTargetOptionsToProto(s.deployTargetOptions(remote)),
		DefaultTarget: protoconv.DeployTargetToProto(defaultTarget(remote)),
		HostTools:     protoconv.HostToolsToProto(deps.HostBuildTools(daemonLookPath)),
		Host:          host,
	}, nil
}

// deployTargetOptions lists, in dialog order, the targets a fleet with this
// remote can use: a file:// template fleet drops the targets that cannot copy
// a template. Each is available when the CLI it needs is on this host's PATH.
// The connected clusters follow, one target each (kube_targets.go).
func (s *service) deployTargetOptions(remote string) []fleet.DeployTargetOption {
	template := fleet.IsTemplateRemote(remote)
	var out []fleet.DeployTargetOption
	for _, target := range fleet.DeployTargets() {
		if template && !target.CopiesTemplates() {
			continue
		}
		option := fleet.DeployTargetOption{Target: target, Available: true}
		if tool := target.Tool(); tool != "" {
			if _, err := daemonLookPath(tool); err != nil {
				option.Available = false
				option.Reason = fmt.Sprintf("%s CLI not found where fleetd runs", tool)
			}
		}
		out = append(out, option)
	}
	return append(out, s.kube.deployTargets(template)...)
}

// rememberedTarget returns the remembered default target (config.json
// DefaultBackend, and DefaultKubeCluster for a cluster target), or the zero
// target when none is recorded, the config can't be read, or it names a
// backend a new instance can't use.
func rememberedTarget() fleet.DeployTarget {
	cfg, err := state.LoadConfig()
	if err != nil {
		return fleet.DeployTarget{}
	}
	backendType := fleet.BackendType(cfg.DefaultBackend)
	if fleet.ValidateBackendType(backendType) != nil {
		return fleet.DeployTarget{}
	}
	return targetFor(backendType, cfg.DefaultKubeCluster)
}

// targetFor builds the target for a backend type and the cluster a request or
// the config names, which only a Kubernetes target carries.
func targetFor(backendType fleet.BackendType, kubeCluster string) fleet.DeployTarget {
	if backendType != fleet.BackendKubernetes {
		kubeCluster = ""
	}
	return fleet.DeployTarget{Backend: backendType, KubeCluster: kubeCluster}
}

// defaultTarget is what an unspecified create in a fleet with this remote
// resolves to: the remembered target, unless there is none or it cannot serve
// the remote (a template fleet only copies onto devcontainer), and then
// devcontainer.
func defaultTarget(remote string) fleet.DeployTarget {
	target := rememberedTarget()
	if target.IsZero() || (fleet.IsTemplateRemote(remote) && !target.CopiesTemplates()) {
		return fleet.DeployTarget{Backend: fleet.BackendDevcontainer}
	}
	return target
}

// resolveTarget turns a create request's target into a concrete one: an
// explicit backend wins (a template fleet still refuses one it can't use, in
// validateTemplateRemote), with the request's cluster for a Kubernetes one
// (the remembered cluster, else the only one, when it names none), and
// UNSPECIFIED takes defaultTarget(remote).
func (s *service) resolveTarget(req *fleetgrpc.CreateInstanceRequest, remote string) fleet.DeployTarget {
	if backendType := protoconv.BackendFromProto(req.GetBackend()); backendType != "" {
		cluster := req.GetKubeCluster()
		if backendType == fleet.BackendKubernetes && cluster == "" {
			remembered := ""
			if cfg, err := state.LoadConfig(); err == nil {
				remembered = cfg.DefaultKubeCluster
			}
			cluster = s.kube.defaultCluster(remembered)
		}
		return targetFor(backendType, cluster)
	}
	return defaultTarget(remote)
}

// rememberDefaultTarget makes target the remembered default. muWrite
// serializes it with SetConfig, the other config.json writer. Writing the
// value already stored is skipped.
func (s *service) rememberDefaultTarget(target fleet.DeployTarget) error {
	if target.IsZero() {
		return nil
	}
	s.muWrite.Lock()
	defer s.muWrite.Unlock()
	cfg, err := state.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.DefaultBackend == string(target.Backend) && cfg.DefaultKubeCluster == target.KubeCluster {
		return nil
	}
	cfg.DefaultBackend = string(target.Backend)
	cfg.DefaultKubeCluster = target.KubeCluster
	return state.SaveConfig(cfg)
}
