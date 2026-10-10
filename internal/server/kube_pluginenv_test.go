//go:build !nok8s

package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestDefaultPluginEnv(t *testing.T) {
	environ := []string{
		"AWS_PROFILE=dev", "AWS_REGION=eu-west-1", "AWS_SECRET_ACCESS_KEY=s3cr3t", "CLOUDSDK_CONFIG=/x", "AZURE_TENANT=t",
		"REQUESTS_CA_BUNDLE=/ca.pem", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/bus", "GITHUB_TOKEN=never", "PATH=/usr/bin",
	}
	for _, tc := range []struct {
		plugin string
		want   string
	}{
		{"aws", "AWS_PROFILE AWS_REGION AWS_SECRET_ACCESS_KEY DBUS_SESSION_BUS_ADDRESS REQUESTS_CA_BUNDLE"},
		{"/usr/local/bin/gke-gcloud-auth-plugin", "CLOUDSDK_CONFIG DBUS_SESSION_BUS_ADDRESS REQUESTS_CA_BUNDLE"},
		{"kubelogin", "AZURE_TENANT DBUS_SESSION_BUS_ADDRESS REQUESTS_CA_BUNDLE"},
		{"some-plugin", "DBUS_SESSION_BUS_ADDRESS REQUESTS_CA_BUNDLE"},
		{"", ""},
	} {
		got := defaultPluginEnv(tc.plugin, environ)
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%q: %v, want %s", tc.plugin, got, tc.want)
		}
		// Names only, so the record holds no value and passes validation.
		if err := fleet.ValidatePluginEnv(got); err != nil {
			t.Errorf("%q: %v", tc.plugin, err)
		}
	}
}

// A connection added without plugin_env passes the plugin's variables
// through by name; one added with plugin_env keeps its own.
func TestAddKubeClusterDefaultsPluginEnv(t *testing.T) {
	f := newKubeFixture(t)
	kubeconfig := writeKubeconfigFile(t, filepath.Join(t.TempDir(), "config"), "", `
- name: eks
  context: {cluster: prod, user: aws}
`, `
- name: aws
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: aws, interactiveMode: Never}}
`)
	f.svc.kube.environ = func() []string { return []string{"PATH=/usr/bin", "AWS_PROFILE=prod", "HOME=" + f.home} }
	st := f.add(&fleetgrpc.KubeCluster{Context: "eks", KubeconfigPaths: []string{kubeconfig}})
	if got := st.GetCluster().GetPluginEnv(); len(got) != 1 || got[0] != "AWS_PROFILE" {
		t.Errorf("plugin_env = %v, want [AWS_PROFILE]", got)
	}
	reply, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{
		Context: "eks", KubeconfigPaths: []string{kubeconfig}, Namespace: "other", PluginEnv: []string{"AWS_REGION=us-east-1"},
	}})
	if err != nil || len(reply.GetCluster().GetCluster().GetPluginEnv()) != 1 || reply.GetCluster().GetCluster().GetPluginEnv()[0] != "AWS_REGION=us-east-1" {
		t.Errorf("explicit plugin_env = %v, %v", reply.GetCluster().GetCluster().GetPluginEnv(), err)
	}
}
