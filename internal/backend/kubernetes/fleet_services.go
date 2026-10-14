//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

var _ fleetlaunch.PeerDomainer = (*Backend)(nil)

// fleet_services.go is what the pool lends the per-fleet objects of a
// connection, which are not any instance's (internal/svcruntime/kubert: a
// fleet's anchor, its sibling DNS, its NetworkPolicies and its caches): the
// connection's current record, its last preflight's facts, and plain execs
// in the pods of fleet's own service Deployments; and what an instance's
// handle tells fleet.rc of its fleet's sibling DNS.

// Record is connection id's record as kube_clusters.json holds it now (see
// currentRecord).
func (p *Pool) Record(id string) (fleet.KubeCluster, error) {
	c, err := p.Cluster(id)
	if err != nil {
		return fleet.KubeCluster{}, err
	}
	return currentRecord(c), nil
}

// ConnectionFacts is connection clusterID's last full preflight, through the
// daemon's environment (an error outside fleetd).
func ConnectionFacts(ctx context.Context, clusterID string) (Facts, error) {
	return currentEnvironment().facts(ctx, clusterID)
}

// ExecService runs argv in container of pod, one of fleet's own service pods
// on connection clusterID (a cache's Deployment), and returns its exit code.
// It is paced like FallbackExec. Unlike an instance's pod, a service pod is
// not verified first: the commands it runs read what fleet already knows
// (how full a cache is) or ask the service for its own upkeep, and carry
// nothing secret, so a substituted pod could only answer them wrongly.
func (p *Pool) ExecService(ctx context.Context, clusterID, pod, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return -1, err
	}
	if err := c.gates.fallback.wait(ctx); err != nil {
		return -1, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	req := kubeexec.ExecRequest{Namespace: c.Namespace(), Pod: pod, Container: container, Command: argv}
	err = p.exec(ctx, c, req, kubeexec.Stdio{Stdout: stdout, Stderr: stderr})
	var exit *kubeexec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.Code, nil
	}
	return -1, classify(err)
}

// PeerDomain is the instance's fleet's headless Service on its connection and
// the connection's namespace (fleetlaunch.PeerDomainer): the instance's
// siblings there resolve as <their pod>.<service>.<namespace>.svc. ok is false
// for a handle that cannot name them (an unbound record, no daemon identity).
func (b *Backend) PeerDomain() (service, namespace string, ok bool) {
	id, err := b.identity()
	if err != nil {
		return "", "", false
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		return "", "", false
	}
	if cid, ok := fleet.ParseKubeContainerID(b.inst.ContainerID); ok {
		namespace = cid.Namespace
	} else if rec, err := LoadInstanceRecord(id.cluster, id.key); err == nil {
		namespace = rec.Namespace
	} else if rec, err := b.p().Record(id.cluster); err == nil {
		namespace = rec.Namespace
	}
	if namespace == "" {
		return "", "", false
	}
	return names.FleetObject(id.fleet, names.FleetHash(ident.DaemonID, id.cluster, id.fleet), names.SuffixNet), namespace, true
}
