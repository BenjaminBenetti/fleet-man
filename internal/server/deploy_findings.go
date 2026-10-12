package server

import (
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// deploy_findings.go is what the inspector's check/kubernetes reads of a
// fleet's devcontainer.json for its cluster Deploy targets
// (ListDeployTargets): a compose devcontainer makes every cluster target
// unavailable, and Docker-in-Docker on a cluster that allows no privileged
// pods, or a prebuilt image on one without root, is a warning on that
// cluster's target. The file is read where provisioning reads it, on the
// daemon host: the fleet's newest checkout there (a template fleet's own
// folder, then its instances' workspaces, as the automation login check
// reads them), else what the last InspectRepo of its remote read (a fleet
// added a moment ago, with no instance yet). A fleet with neither has no
// findings; a create's own preflight still refuses what it must.

// repoCheckLimit bounds repoChecks: past it, the inspections kept so far are
// dropped (remotes inspected but never added stay out of the way).
const repoCheckLimit = 256

// repoChecks is what InspectRepo's check/kubernetes read of each remote.
type repoChecks struct {
	mu       sync.Mutex
	byRemote map[string]kubecheck.Config
}

func (r *repoChecks) put(remote string, cfg kubecheck.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byRemote == nil || len(r.byRemote) >= repoCheckLimit {
		r.byRemote = map[string]kubecheck.Config{}
	}
	r.byRemote[remote] = cfg
}

func (r *repoChecks) get(remote string) (kubecheck.Config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.byRemote[remote]
	return cfg, ok
}

// fleetRepoConfig is what check/kubernetes reads of fleet f's
// devcontainer.json; the zero Config (no findings) without a fleet, or with
// no readable configuration on this host and no inspection of its remote.
func (s *service) fleetRepoConfig(f *fleet.Fleet) kubecheck.Config {
	if f == nil {
		return kubecheck.Config{}
	}
	for _, dir := range devcontainerDirs(f) {
		if cfg, err := kubecheck.Read(&inspector.Repo{Root: dir, Keep: true}); err == nil {
			return cfg
		}
	}
	cfg, _ := s.repoChecks.get(f.Remote)
	return cfg
}

// deployAudioWanted reports whether either audio direction is on (the
// daemon's settings), which is when a cluster without root's missing audio
// concerns a create; an unreadable config counts as on (audio output is on
// by default), as the create's own check takes it.
func deployAudioWanted() bool {
	cfg, err := state.LoadConfig()
	return err != nil || cfg.MicSettings.Enabled || cfg.OutputSettings.Enabled
}
