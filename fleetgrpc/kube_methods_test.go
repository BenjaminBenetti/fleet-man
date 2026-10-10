package fleetgrpc

import (
	"strings"
	"testing"
)

func TestKubeClusterStateDisplay(t *testing.T) {
	cases := map[KubeClusterState]string{
		KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:     "unknown",
		KubeClusterState_KUBE_CLUSTER_STATE_CHECKING:        "checking",
		KubeClusterState_KUBE_CLUSTER_STATE_READY:           "ready",
		KubeClusterState_KUBE_CLUSTER_STATE_LIMITED:         "limited",
		KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE:     "unreachable",
		KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED:    "unauthorized",
		KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN:       "forbidden",
		KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING: "context missing",
		KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING:  "plugin missing",
		KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED:   "misconfigured",
		KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT:  "owner conflict",
		KubeClusterState(99):                                "unknown",
	}
	for s, want := range cases {
		if got := s.Display(); got != want {
			t.Errorf("%v.Display() = %q, want %q", s, got, want)
		}
	}
	for num := range KubeClusterState_name {
		if _, ok := cases[KubeClusterState(num)]; !ok {
			t.Errorf("KubeClusterState %v has no Display case", KubeClusterState(num))
		}
	}
}

func TestKubeClusterStateUsable(t *testing.T) {
	for num := range KubeClusterState_name {
		s := KubeClusterState(num)
		want := s == KubeClusterState_KUBE_CLUSTER_STATE_READY || s == KubeClusterState_KUBE_CLUSTER_STATE_LIMITED
		if s.Usable() != want {
			t.Errorf("%v.Usable() = %v, want %v", s, s.Usable(), want)
		}
	}
}

func TestKubeCheckResultDisplay(t *testing.T) {
	cases := map[KubeCheckResult]string{
		KubeCheckResult_KUBE_CHECK_RESULT_UNSPECIFIED: "unknown",
		KubeCheckResult_KUBE_CHECK_RESULT_RUNNING:     "running",
		KubeCheckResult_KUBE_CHECK_RESULT_PASS:        "pass",
		KubeCheckResult_KUBE_CHECK_RESULT_WARN:        "warn",
		KubeCheckResult_KUBE_CHECK_RESULT_FAIL:        "fail",
		KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED:     "skipped",
	}
	for r, want := range cases {
		if got := r.Display(); got != want {
			t.Errorf("%v.Display() = %q, want %q", r, got, want)
		}
	}
	for num := range KubeCheckResult_name {
		if _, ok := cases[KubeCheckResult(num)]; !ok {
			t.Errorf("KubeCheckResult %v has no Display case", KubeCheckResult(num))
		}
	}
}

func TestKubeClusterDisplayNameOrID(t *testing.T) {
	if got := (&KubeCluster{Id: "minikube-a1b2c3", DisplayName: "dev"}).DisplayNameOrID(); got != "dev" {
		t.Errorf("named = %q", got)
	}
	if got := (&KubeCluster{Id: "minikube-a1b2c3"}).DisplayNameOrID(); got != "minikube-a1b2c3" {
		t.Errorf("unnamed = %q", got)
	}
}

func TestFindKubeCluster(t *testing.T) {
	status := func(id, name, context string) *KubeClusterStatus {
		return &KubeClusterStatus{Cluster: &KubeCluster{Id: id, DisplayName: name, Context: context}}
	}
	clusters := []*KubeClusterStatus{
		status("minikube-a1b2c3", "dev", "minikube"),
		status("prod-d4e5f6", "Prod-EU", "gke_acme_prod"),
		status("prod-g7h8i9", "", "gke_acme_prod"),
		// A display name that is another connection's id: the id wins.
		status("odd-j1k2l3", "minikube-a1b2c3", "kind-kind"),
	}
	for _, tc := range []struct {
		ref, want, err string
	}{
		{ref: "minikube-a1b2c3", want: "minikube-a1b2c3"},
		{ref: "dev", want: "minikube-a1b2c3"},
		{ref: "prod-eu", want: "prod-d4e5f6"},
		{ref: "PROD-EU", want: "prod-d4e5f6"},
		{ref: "minikube", want: "minikube-a1b2c3"},
		{ref: "kind-kind", want: "odd-j1k2l3"},
		{ref: "prod-g7h8i9", want: "prod-g7h8i9"},
		{ref: "gke_acme_prod", err: `2 connections use context "gke_acme_prod" (Prod-EU, prod-g7h8i9): name one`},
		{ref: "nope", err: `no Kubernetes connection "nope"`},
		{ref: "", err: "no cluster named"},
		// Context names match exactly; display names in any case.
		{ref: "MINIKUBE", err: `no Kubernetes connection "MINIKUBE"`},
	} {
		got, err := FindKubeCluster(clusters, tc.ref)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("FindKubeCluster(%q) = %v, %v; want error %q", tc.ref, got, err, tc.err)
			}
			continue
		}
		if err != nil || got.GetCluster().GetId() != tc.want {
			t.Errorf("FindKubeCluster(%q) = %v, %v; want %s", tc.ref, got.GetCluster().GetId(), err, tc.want)
		}
	}
}
