//go:build !nok8s

package kubeexec

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// LiveContextEnv names the kube context of a real cluster the live tests of
// kubeexec and k8s/owner run against; unset, they skip. They read the
// daemon-default kubeconfig ($KUBECONFIG or ~/.kube/config); k8s/owner's
// creates one fleet-it-p1-01-owner-* namespace and deletes it.
const LiveContextEnv = "FLEET_KUBE_TEST_CONTEXT"

// A real kubeconfig context loads pinned, its client talks to the API
// server, and its credential's expiry is read.
func TestLiveConnection(t *testing.T) {
	kctx := os.Getenv(LiveContextEnv)
	if kctx == "" {
		t.Skip(LiveContextEnv + " is not set")
	}
	paths := DefaultKubeconfigPaths(os.Environ())
	contexts, err := ListContexts(paths)
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	var found *KubeContext
	for i := range contexts {
		if contexts[i].Name == kctx {
			found = &contexts[i]
		}
	}
	if found == nil {
		t.Fatalf("context %q not in %v", kctx, contexts)
	}

	conn, err := Load(fleet.KubeCluster{ID: "live-abc234", Context: kctx, KubeconfigPaths: paths, Namespace: "default"}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if conn.Server != found.Server || conn.Auth != found.Auth {
		t.Errorf("connection %s/%s, context listing %s/%s", conn.Server, conn.Auth, found.Server, found.Auth)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	t.Logf("server %s %s, auth %s", conn.Server, v.GitVersion, conn.Auth)
	cred, err := conn.Credential(ctx)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	t.Logf("credential %+v", cred)
	if conn.Auth == AuthClientCert && !cred.ExpiresAt.After(time.Now()) {
		t.Errorf("client certificate expiry %v is not in the future", cred.ExpiresAt)
	}
}
