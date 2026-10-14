package kubeimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// Secrets manages a connection's managed pull Secret in its namespace. The
// cluster backend implements it with server-side apply, binding the
// connection's client and namespace.
type Secrets interface {
	// ApplyPullSecret creates or updates the Secret (Manifest, applied
	// server-side), never recreating it.
	ApplyPullSecret(ctx context.Context, s PullSecret) error
	// DeletePullSecret deletes it; a Secret that is not there is not an
	// error.
	DeletePullSecret(ctx context.Context, name string) error
}

// PullSecret is a connection's managed pull Secret: one registry's
// credential from the daemon host, scoped to that registry alone; never the
// rest of the host's docker logins.
type PullSecret struct {
	Name   string
	Labels map[string]string
	// Registry is the registry domain the credential is for.
	Registry string
	// DockerConfigJSON is the Secret's .dockerconfigjson.
	DockerConfigJSON []byte
}

// pullSecretType is the Secret type the kubelet reads pull credentials from.
const pullSecretType = "kubernetes.io/dockerconfigjson"

// Manifest is the Secret as a JSON object for server-side apply, without a
// namespace (the applier sets the connection's).
func (s PullSecret) Manifest() ([]byte, error) {
	if s.Name == "" || len(s.DockerConfigJSON) == 0 {
		return nil, fmt.Errorf("pull secret: a name and a credential are needed")
	}
	type meta struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	obj := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   meta              `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: meta{
			Name:        s.Name,
			Labels:      maps.Clone(s.Labels),
			Annotations: map[string]string{pullSecretRegistryAnnotation: s.Registry},
		},
		Type: pullSecretType,
		Data: map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(s.DockerConfigJSON)},
	}
	return json.Marshal(obj)
}

// pullSecretRegistryAnnotation names the registry a managed pull Secret is
// for (a non-secret field).
const pullSecretRegistryAnnotation = "fleet.dev/registry"

// ensurePullSecret brings the connection's managed pull Secret in line for
// a registry the pods pull from: the daemon host's credential for it when
// the connection manages one and the host has a usable login, else no
// Secret (an old one is deleted, so no stale copy of a credential stays in
// the cluster). It returns the Secret's name when there is one, and a
// warning when the login cannot be used.
func (m *Manager) ensurePullSecret(ctx context.Context, t Target, domain string) (string, string, error) {
	name := PullSecretName(t.DaemonID, t.Cluster.ID)
	if t.Secrets == nil {
		if t.Cluster.Image.ManagesPullSecret() {
			return "", "", fmt.Errorf("image delivery: no namespace to keep the managed pull Secret in")
		}
		return "", "", nil
	}
	if !t.Cluster.Image.ManagesPullSecret() {
		return "", "", t.Secrets.DeletePullSecret(ctx, name)
	}
	cred, err := m.credentials().Lookup(ctx, domain)
	if err != nil {
		return "", "", fmt.Errorf("reading the daemon host's login to %s: %w", domain, err)
	}
	if cred.Empty() {
		return "", "", t.Secrets.DeletePullSecret(ctx, name)
	}
	data, err := dockerConfigJSON(domain, cred)
	if err != nil {
		if derr := t.Secrets.DeletePullSecret(ctx, name); derr != nil {
			return "", "", derr
		}
		return "", "pods pull from " + domain + " without a managed pull Secret: " + err.Error(), nil
	}
	s := PullSecret{Name: name, Labels: t.Labels, Registry: domain, DockerConfigJSON: data}
	if err := t.Secrets.ApplyPullSecret(ctx, s); err != nil {
		return "", "", fmt.Errorf("applying the pull Secret %s: %w", name, err)
	}
	return name, "", nil
}

// RefreshPullSecret re-applies the managed pull Secret from the daemon
// host's current login, so a short-lived one (ECR, a cloud helper's token)
// stays usable for pods the cluster reschedules without fleet. Every GC pass
// calls it; it does nothing for a connection without a registry or without
// Secrets.
func (m *Manager) RefreshPullSecret(ctx context.Context, t Target) error {
	if err := t.validate(); err != nil {
		return err
	}
	reg, ok, err := t.registry()
	if err != nil || !ok || t.Secrets == nil {
		return err
	}
	_, warning, err := m.ensurePullSecret(ctx, t, reg.Domain)
	if warning != "" && err == nil {
		return fmt.Errorf("%s", warning)
	}
	return err
}

// pullSecretsFor lists the imagePullSecrets of a pod pulling from domain:
// the connection's configured Secrets, then the managed one when domain is
// the connection's registry.
func (m *Manager) pullSecretsFor(ctx context.Context, t Target, domain string) ([]string, []string, error) {
	var names, warnings []string
	for _, n := range t.Cluster.Image.PullSecrets {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if reg, ok, _ := t.registry(); ok && reg.Domain == domain {
		name, warning, err := m.ensurePullSecret(ctx, t, domain)
		if err != nil {
			return nil, nil, err
		}
		if name != "" {
			names = append(names, name)
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return names, warnings, nil
}
