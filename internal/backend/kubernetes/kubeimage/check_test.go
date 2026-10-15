package kubeimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

func TestCheckRegistry(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		auth authMode
		mode deleteMode
		// pushed: fleet pushed an image to the registry before the check.
		pushed    bool
		supported bool
		untested  bool
		why       string
	}{
		{"delete enabled", authNone, deleteEnabled, false, true, false, ""},
		{"delete enabled behind a token", authBearer, deleteEnabled, false, true, false, ""},
		{"delete disabled", authBasic, deleteDisabled, false, false, false, "does not allow deleting images (HTTP 405)"},
		{"login may not delete", authNone, deleteDenied, true, false, false, "may not delete images (HTTP 403)"},
		// Before fleet's first push, a refusal may only mean that the
		// daemon's repository does not exist yet.
		{"refused before the first push", authNone, deleteDenied, false, false, true, ""},
		{"deletes only in a repository that exists", authBearer, deleteNeedsRepo, false, false, true, ""},
		{"deletes once fleet has pushed", authBearer, deleteNeedsRepo, true, true, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.reg.auth, h.reg.delete = tt.auth, tt.mode
			h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
			tgt := h.target(h.registryCluster(), localcluster.Cluster{})
			if tt.pushed {
				h.seedPush(t, "app")
				if _, err := h.m.Deliver(ctx, tgt, builtImage(imageID("app")), "inst-a"); err != nil {
					t.Fatal(err)
				}
			}
			check, err := h.m.CheckRegistry(ctx, tgt)
			if err != nil {
				t.Fatal(err)
			}
			if !check.Reachable || check.DeleteSupported != tt.supported || check.DeleteUntested != tt.untested || !strings.Contains(check.Reason, tt.why) || tt.why == "" && check.Reason != "" ||
				check.Registry != "registry.example.com/team" || !check.CheckedAt.Equal(h.clock.Now()) {
				t.Errorf("CheckRegistry = %+v", check)
			}
			// The probe deletes nothing real, inside the daemon's own path
			// (asked twice behind a login: the challenge, then the retry).
			if h.reg.count("DELETE ") == 0 || h.reg.count("DELETE ") != h.reg.count("DELETE /v2/"+ownRepoPath+"/manifests/"+zeroDigest) {
				t.Errorf("registry saw %v", h.reg.seen())
			}
			// An untested answer leaves the GC to check again.
			st := h.indexOf(t, tgt.Cluster.ID).Registries["registry.example.com"]
			if tt.untested && st.DeleteSupported != nil || !tt.untested && (st.DeleteSupported == nil || *st.DeleteSupported != tt.supported) || st.Pushed != tt.pushed {
				t.Errorf("index state %+v", st)
			}

			s := h.m.Status(ctx, tgt)
			if s.Mode != "registry" || !s.RegistryReachable || s.RegistryDeleteSupported != tt.supported || s.RegistryDeleteUntested != tt.untested {
				t.Errorf("Status = %+v", s)
			}
		})
	}

	t.Run("an index from before the pushed flag", func(t *testing.T) {
		ix := &Index{Images: []Entry{{Strategy: StrategyRegistry, Location: "registry.example.com/" + ownRepoPath}}}
		if !ix.pushedTo("registry.example.com") || ix.pushedTo("other.example.com") {
			t.Error("pushedTo misread the index's entries")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		h := newHarness(t)
		h.m.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") })
		check, err := h.m.CheckRegistry(ctx, h.target(h.registryCluster(), localcluster.Cluster{}))
		if err != nil || check.Reachable || check.DeleteSupported || !strings.Contains(check.Reason, "cannot reach registry.example.com") {
			t.Errorf("CheckRegistry = %+v, %v", check, err)
		}
	})

	t.Run("no registry", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.m.CheckRegistry(ctx, h.target(h.cluster(), localcluster.Cluster{})); err == nil {
			t.Error("checked a registry the connection does not have")
		}
		s := h.m.Status(ctx, h.target(h.cluster(), minikubeDev))
		if s.Mode != "node-load" || s.RegistryReachable || !strings.Contains(s.Reason, "local cluster") {
			t.Errorf("Status = %+v", s)
		}
		s = h.m.Status(ctx, h.target(h.cluster(), localcluster.Cluster{}))
		if s.Mode != "none" || !strings.Contains(s.Reason, "Settings") {
			t.Errorf("Status = %+v", s)
		}
	})
}

func TestPullSecretManifest(t *testing.T) {
	cfg, _ := dockerConfigJSON("ghcr.io", Credential{Username: "u", Password: "p"})
	s := PullSecret{Name: "fleet-pull-x-dev", Labels: map[string]string{"app.kubernetes.io/managed-by": "fleet"}, Registry: "ghcr.io", DockerConfigJSON: cfg}
	b, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(obj.Data[".dockerconfigjson"])
	if obj.APIVersion != "v1" || obj.Kind != "Secret" || obj.Type != "kubernetes.io/dockerconfigjson" || obj.Metadata.Name != s.Name ||
		obj.Metadata.Namespace != "" || obj.Metadata.Labels["app.kubernetes.io/managed-by"] != "fleet" ||
		obj.Metadata.Annotations["fleet.dev/registry"] != "ghcr.io" || string(raw) != string(cfg) || len(obj.Data) != 1 {
		t.Errorf("manifest %s", b)
	}
	if _, err := (PullSecret{Name: "x"}).Manifest(); err == nil {
		t.Error("a Secret without a credential")
	}
}

func TestRefreshPullSecret(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.m.RefreshPullSecret(ctx, h.target(h.cluster(), localcluster.Cluster{})); err != nil || len(h.secrets.applied)+len(h.secrets.deletes) != 0 {
		t.Errorf("without a registry: %v", err)
	}
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p1"}
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	if err := h.m.RefreshPullSecret(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p2"}
	if err := h.m.RefreshPullSecret(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	s, _ := h.secrets.get(PullSecretName(testDaemonID, tgt.Cluster.ID))
	if !strings.Contains(string(s.DockerConfigJSON), `"password":"p2"`) {
		t.Errorf("secret %s", s.DockerConfigJSON)
	}
	h.creds["registry.example.com"] = Credential{IdentityToken: "t"}
	if err := h.m.RefreshPullSecret(ctx, tgt); err == nil || !strings.Contains(err.Error(), "kubelet cannot use") {
		t.Errorf("an unusable login: %v", err)
	}
	if _, ok := h.secrets.get(PullSecretName(testDaemonID, tgt.Cluster.ID)); ok {
		t.Error("the stale Secret stayed")
	}
	_ = fleet.KubeImageRegistry
}
