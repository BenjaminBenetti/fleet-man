package kubeimage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RegistryCheck is what a check of a connection's registry found.
type RegistryCheck struct {
	// Registry is the registry setting checked.
	Registry string
	// Reachable: the registry answered its /v2/ endpoint.
	Reachable bool
	// DeleteSupported: the registry deletes manifests in the daemon's
	// repository with the daemon host's login. Without it, images past
	// retention stay in the registry (the cluster editor says so).
	DeleteSupported bool
	// Reason says why not, when either is false.
	Reason    string
	CheckedAt time.Time
}

// zeroDigest names no manifest: deleting it asks a registry whether it
// would delete, without deleting anything.
const zeroDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// deleteRecheck is how long a registry's DELETE support is trusted before
// the GC checks it again.
const deleteRecheck = 24 * time.Hour

// CheckRegistry checks the connection's registry: whether it answers, and
// whether it deletes manifests in the daemon's own repository. It asks for
// the deletion of a digest no manifest has: a registry that deletes answers
// 404 (MANIFEST_UNKNOWN), one that does not answers 405 (UNSUPPORTED), and
// one that refuses the login 401 or 403. The result is kept in the index for
// the GC. A connection without a registry is an error.
func (m *Manager) CheckRegistry(ctx context.Context, t Target) (RegistryCheck, error) {
	if err := t.validate(); err != nil {
		return RegistryCheck{}, err
	}
	reg, ok, err := t.registry()
	if err != nil {
		return RegistryCheck{}, err
	}
	if !ok {
		return RegistryCheck{}, errors.New("no registry set")
	}
	check := RegistryCheck{Registry: reg.String(), CheckedAt: m.now()}
	c, err := m.client(ctx, t, reg.Domain)
	if err != nil {
		check.Reason = err.Error()
		return check, nil
	}
	if err := c.ping(ctx); err != nil {
		check.Reason = fmt.Sprintf("the daemon host cannot reach %s: %v", reg.Domain, err)
		return check, nil
	}
	check.Reachable = true
	check.DeleteSupported, check.Reason = probeDelete(ctx, c, ownPath(reg.Prefix, t.DaemonID))
	if path, err := m.indexPath(t); err == nil {
		supported := check.DeleteSupported
		if err := m.files.update(path, func(ix *Index) error {
			if ix.Registries == nil {
				ix.Registries = map[string]RegistryState{}
			}
			ix.Registries[reg.Domain] = RegistryState{DeleteSupported: &supported, Reason: check.Reason, CheckedAt: check.CheckedAt}
			return nil
		}); err != nil {
			return check, err
		}
	}
	return check, nil
}

// probeDelete asks the registry to delete a manifest that does not exist in
// repoPath and reads the answer.
func probeDelete(ctx context.Context, c *registryClient, repoPath string) (bool, string) {
	err := c.deleteManifest(ctx, repoPath, zeroDigest)
	if err == nil || isNotFound(err) {
		return true, ""
	}
	var re *RegistryError
	if !errors.As(err, &re) {
		return false, fmt.Sprintf("could not tell whether %s deletes images: %v", c.domain, err)
	}
	switch {
	case re.Status == http.StatusMethodNotAllowed || re.has("UNSUPPORTED"):
		return false, fmt.Sprintf("%s does not allow deleting images (HTTP %d): images past retention stay in the registry", c.domain, re.Status)
	case isDenied(err):
		return false, fmt.Sprintf("the daemon host's login to %s may not delete images (HTTP %d): images past retention stay in the registry", c.domain, re.Status)
	}
	return false, fmt.Sprintf("could not tell whether %s deletes images: %v", c.domain, err)
}

// DeliveryStatus is how a connection's images are delivered, for its
// health: what Resolve picked and what the registry check found.
type DeliveryStatus struct {
	// Mode is the resolved strategy ("node-load", "registry", "gateway",
	// "none").
	Mode string
	// RegistryReachable and RegistryDeleteSupported are set when the mode is
	// registry.
	RegistryReachable       bool
	RegistryDeleteSupported bool
	// Reason says why Auto picked the mode, why nothing delivers, or what is
	// wrong with the registry.
	Reason string
}

// Status resolves the connection's delivery and, for registry delivery,
// checks the registry: the image delivery line of the connection's health.
// The check goes to the registry and records its answer in the index, so it
// belongs in the full preflight, not the frequent shallow probe.
func (m *Manager) Status(ctx context.Context, t Target) DeliveryStatus {
	res := m.Resolve(t)
	st := DeliveryStatus{Mode: string(res.Strategy), Reason: res.Reason}
	if res.Strategy != StrategyRegistry {
		return st
	}
	check, err := m.CheckRegistry(ctx, t)
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	st.RegistryReachable, st.RegistryDeleteSupported = check.Reachable, check.DeleteSupported
	if check.Reason != "" {
		st.Reason = check.Reason
	}
	return st
}
