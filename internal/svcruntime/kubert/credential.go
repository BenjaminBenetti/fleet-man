//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// credential.go is the instances' side of a fleet's shared Buildkit on a
// connection (svcruntime.CredentialIssuer): each instance's client
// certificate (tls.go) issued, recorded once it is in the instance, and how
// long the one it holds is good for, which the daemon's daily renewal and
// the instance's expiry banner go by.

// fleetInstanceKeys are the keys of fleetName's instances whose records name
// connection clusterID, by instance name (state.json). A package var for
// tests.
var fleetInstanceKeys = func(fleetName, clusterID string) (map[string]string, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	f, ok := st.Fleets[fleetName]
	if !ok || f == nil {
		return out, nil
	}
	for _, inst := range f.Instances {
		if inst != nil && inst.Backend == fleet.BackendKubernetes && inst.KubeCluster == clusterID && inst.KubeKey != "" {
			out[inst.Name] = inst.KubeKey
		}
	}
	return out, nil
}

// instanceKey is the key of fleetName/instanceName, whose record must name
// the connection.
func (r *Runtime) instanceKey(fleetName, instanceName string) (string, map[string]string, error) {
	keys, err := fleetInstanceKeys(fleetName, r.cluster)
	if err != nil {
		return "", nil, fmt.Errorf("reading the instance's record: %w", err)
	}
	key, ok := keys[instanceName]
	if !ok {
		return "", nil, fmt.Errorf("no record of %s/%s names connection %s", fleetName, instanceName, r.cluster)
	}
	return key, keys, nil
}

// fleetHash is fleetName's h8 on the connection.
func (r *Runtime) fleetHash(fleetName string) (string, error) {
	id, err := daemonID()
	if err != nil {
		return "", fmt.Errorf("the daemon's identity: %w", err)
	}
	return names.FleetHash(id, r.cluster, fleetName), nil
}

// errCredentialService refuses a credential for a service that takes none.
var errCredentialService = errors.New("kubernetes runtime: only the shared Buildkit takes an instance's credential")

// IssueCredential issues instanceName its client certificate for the fleet's
// shared Buildkit on the connection (CN: the instance's key), and logs it.
// A connection that does not run the server answers BuildkitRefusedError.
// When the daemon's CA or the server's certificate had to be made again (a
// first server, a lost CA file, a certificate near its end), the server gets
// the new one first.
func (r *Runtime) IssueCredential(fleetName, instanceName string, svc fleet.SharedService) (fleet.ServiceCredential, error) {
	if svc != fleet.ServiceBuildkit {
		return fleet.ServiceCredential{}, errCredentialService
	}
	unlock := r.lock(fleetName)
	defer unlock()
	key, _, err := r.instanceKey(fleetName, instanceName)
	if err != nil {
		return fleet.ServiceCredential{}, err
	}
	c, err := r.connect()
	if err != nil {
		return fleet.ServiceCredential{}, err
	}
	ctx, cancel := opContext()
	defer cancel()
	if refused := c.buildkitRefusal(ctx); refused != nil {
		return fleet.ServiceCredential{}, refused
	}
	m := c.meta(fleetName)
	ca, changed, err := ensureCA(c.cluster, m, now())
	if err != nil {
		return fleet.ServiceCredential{}, fmt.Errorf("the fleet's Buildkit CA on %s: %w", c.rec.GetDisplayName(), err)
	}
	if changed {
		if err := c.ensureService(ctx, m, svc); err != nil {
			return fleet.ServiceCredential{}, fmt.Errorf("the %s on %s: %w", serviceText(svc), c.rec.GetDisplayName(), err)
		}
		watchReady(c, m, svc)
	}
	cred, err := ca.issueClient(key, now())
	if err != nil {
		return fleet.ServiceCredential{}, fmt.Errorf("issuing the instance's Buildkit certificate: %w", err)
	}
	flog.Info("kube buildkit: issued an instance's client certificate", "cluster", c.cluster, "fleet", fleetName, "instance", instanceName,
		"cn", key, "serial", cred.Serial, "not_after", cred.NotAfter.UTC().Format(time.RFC3339))
	return cred, nil
}

// CredentialInstalled records that instanceName holds cred, and forgets
// the certificates of instances no record names on the connection any more.
// A credential of a CA the fleet no longer has there (rotated meanwhile) is
// refused.
func (r *Runtime) CredentialInstalled(fleetName, instanceName string, svc fleet.SharedService, cred fleet.ServiceCredential) error {
	if svc != fleet.ServiceBuildkit {
		return errCredentialService
	}
	unlock := r.lock(fleetName)
	defer unlock()
	key, keys, err := r.instanceKey(fleetName, instanceName)
	if err != nil {
		return err
	}
	hash, err := r.fleetHash(fleetName)
	if err != nil {
		return err
	}
	ca, err := loadCA(r.cluster, hash)
	if err != nil {
		return err
	}
	if ca == nil || ca.CACert != string(cred.CA) {
		return fmt.Errorf("the certificate is of a Buildkit CA the fleet no longer has on connection %s", r.cluster)
	}
	live := map[string]bool{}
	for _, k := range keys {
		live[k] = true
	}
	for k := range ca.Installed {
		if !live[k] {
			delete(ca.Installed, k)
		}
	}
	if ca.Installed == nil {
		ca.Installed = map[string]installedCert{}
	}
	ca.Installed[key] = installedCert{Instance: instanceName, Serial: cred.Serial, NotBefore: cred.NotBefore, NotAfter: cred.NotAfter}
	return saveCA(r.cluster, hash, ca)
}

// InstalledCredential is the validity of the certificate last recorded in
// instanceName (ok false: none of the fleet's current CA there is).
func (r *Runtime) InstalledCredential(fleetName, instanceName string, svc fleet.SharedService) (notBefore, notAfter time.Time, ok bool) {
	if svc != fleet.ServiceBuildkit {
		return time.Time{}, time.Time{}, false
	}
	key, _, err := r.instanceKey(fleetName, instanceName)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	hash, err := r.fleetHash(fleetName)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	ca, err := loadCA(r.cluster, hash)
	if err != nil || ca == nil {
		return time.Time{}, time.Time{}, false
	}
	ic, ok := ca.Installed[key]
	return ic.NotBefore, ic.NotAfter, ok
}

// refreshBuildkit issues the server a new certificate when its one nears its
// end (the CA too, near its own), and gives it to the server: the daily
// upkeep, which a server running a year without a create or a TUI connect
// would otherwise outlive. Like EnsureService it makes nothing for a fleet
// that left the connection after the upkeep read the state: a CA rotated
// with its teardown would be made again, and its server with it.
func (r *Runtime) refreshBuildkit(ctx context.Context, c *conn, m fleetMeta) error {
	unlock := r.lock(m.Fleet)
	defer unlock()
	if r.leftCluster(m.Fleet, "refresh buildkit") || c.buildkitRefusal(ctx) != nil {
		return nil
	}
	if _, changed, err := ensureCA(c.cluster, m, now()); err != nil || !changed {
		return err
	}
	if err := c.ensureService(ctx, m, fleet.ServiceBuildkit); err != nil {
		return err
	}
	watchReady(c, m, fleet.ServiceBuildkit)
	flog.Info("kube buildkit: the server has a new certificate", "cluster", c.cluster, "fleet", m.Fleet)
	return nil
}
