//go:build !nok8s

package kubernetes

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
)

// editor.go: VS Code opens a cluster instance over Remote-SSH, through the
// instance's `*.fleet` ssh alias, which ~/.ssh/fleet_config routes through
// `fleet ssh-proxy` and the daemon's Exec stream to the pod's stdio-only
// `fleet ssh-server`. The editor machine needs no kubeconfig.

var _ backend.EditorTarget = (*Backend)(nil)

// EditorURI opens the instance's workspace folder over Remote-SSH, through
// its alias on this daemon (`<instance>.<fleet>.fleet`; a client of a
// remote daemon adds the remote's part). False while fleetd has no record
// of the instance's pod.
func (b *Backend) EditorURI(string, string) (string, bool) {
	id, err := b.identity()
	if err != nil {
		return "", false
	}
	folder, err := b.workspaceFolder()
	if err != nil {
		return "", false
	}
	return sshalias.URI(sshalias.Alias(id.fleet, id.name, ""), folder), true
}

// EditorInfo reads what VS Code applies on attach (backend.EditorTarget):
// the workspace folder from fleetd's record of the instance, and the
// devcontainer.metadata label from the container the create captured,
// kept in the root generation's spec Secret.
func (b *Backend) EditorInfo(ctx context.Context, _ string) (backend.EditorInfo, error) {
	id, err := b.identity()
	if err != nil {
		return backend.EditorInfo{}, err
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return backend.EditorInfo{}, err
	}
	c, err := b.p().inspectedContainer(ctx, rec.Cluster, rec.StatefulSet, rec.Gen)
	if err != nil {
		return backend.EditorInfo{}, err
	}
	return backend.EditorInfo{WorkspaceFolder: rec.WorkspaceFolder, Metadata: c.Labels[devmeta.MetadataLabel]}, nil
}
