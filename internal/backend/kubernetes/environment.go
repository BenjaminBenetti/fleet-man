//go:build !nok8s

package kubernetes

import (
	"context"
	"os"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// environment.go is what the daemon gives the backend handles beyond the
// pool: its one image delivery (the kubeimage.Manager every connection shares,
// with the gateway image server behind it, which also explains failed pulls
// through it) and what each connection's last
// full preflight found (the facts a translation needs: is the cluster local,
// its nodes and their platform, what admission lets in). fleetd sets it when
// it starts (SetEnvironment). A process without one (a test, or a CLI that
// acts on an instance in-process) can stop, start and tear instances down,
// but only fleetd creates them.

// Facts is what a connection's last full preflight found.
type Facts struct {
	// Local is the local cluster the nodes belong to (node load reaches it),
	// Platform the nodes' one os/arch ("linux/amd64"; "" when mixed or
	// unknown).
	Local    localcluster.Cluster
	Platform string
	// Capabilities are the preflight's findings.
	Capabilities *fleetgrpc.KubeCapabilities
}

// Environment is the daemon's side of the backend.
type Environment struct {
	// Images delivers instance images; nil in a process without the daemon's
	// (Down and Start then use one of their own on the same index).
	Images *kubeimage.Manager
	// Gateway is the gateway image server, which explains a failed pull of
	// a gateway-delivered image; nil when the daemon serves none.
	Gateway GatewayPulls
	// Facts is connection clusterID's last full preflight, waited for (within
	// ctx) when none has finished yet; an error when the connection is
	// unusable, with its kubernetes: code.
	Facts func(ctx context.Context, clusterID string) (Facts, error)
	// Tools are the programs capture-up and set-up run (the zero value:
	// the devcontainer CLI and docker on PATH, the daemon's own executable
	// as the docker shim).
	Tools devcli.Tools
	// ShareReady is the shared-state Ready gate of a Start (the daemon's
	// SharedStateReady, which a create, clone or rebuild gets through its
	// job's hooks instead, backend.HooksFrom): it waits for the fleet's
	// logins and settings in the instance's pod before the start hooks run;
	// an error is a warning. nil skips it.
	ShareReady func(ctx context.Context, fleetName, instance string) error
}

var (
	envMu sync.Mutex
	env   Environment
)

// SetEnvironment installs the daemon's environment (fleetd, at start).
func SetEnvironment(e Environment) {
	envMu.Lock()
	env = e
	envMu.Unlock()
}

func currentEnvironment() Environment {
	envMu.Lock()
	e := env
	envMu.Unlock()
	if e.Tools.Exe == "" {
		e.Tools.Exe = executableOverride()
	}
	return e
}

var (
	exeMu       sync.Mutex
	exeOverride string
)

// SetFleetExecutable names the fleet executable the backend hands out to run
// (the `fleet kube-exec` argvs, capture-up's docker shim) for a process that
// is not itself the fleet binary: the conformance suite runs fleetd in its
// test binary. "" puts back the default, this process's own executable.
func SetFleetExecutable(path string) {
	exeMu.Lock()
	exeOverride = path
	exeMu.Unlock()
}

func executableOverride() string {
	exeMu.Lock()
	defer exeMu.Unlock()
	return exeOverride
}

// fleetExecutable is the fleet executable the backend's argvs run.
func fleetExecutable() (string, error) {
	if p := executableOverride(); p != "" {
		return p, nil
	}
	return os.Executable()
}

// images is the environment's image delivery, or one of this process's own
// on the daemon's index (for a release or a re-delivery outside fleetd).
func (e Environment) images() *kubeimage.Manager {
	if e.Images != nil {
		return e.Images
	}
	return &kubeimage.Manager{IndexPath: state.KubeImagesPath}
}

// errNotDaemon refuses a create outside fleetd.
var errNotDaemon = kubeerr.New(kubeerr.Unavailable, "only the fleet daemon creates Kubernetes instances")

// facts is connection clusterID's facts through the environment.
func (e Environment) facts(ctx context.Context, clusterID string) (Facts, error) {
	if e.Facts == nil {
		return Facts{}, errNotDaemon
	}
	return e.Facts(ctx, clusterID)
}
