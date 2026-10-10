package localcluster

import "testing"

func TestDetect(t *testing.T) {
	minikube := func(profile, name string) Node {
		return Node{Name: name, Labels: map[string]string{"minikube.k8s.io/name": profile, "kubernetes.io/arch": "amd64"}}
	}
	kind := func(cluster, name string) Node {
		return Node{Name: name, ProviderID: "kind://docker/" + cluster + "/" + name}
	}
	tests := []struct {
		name  string
		nodes []Node
		want  Cluster
	}{
		{"no nodes", nil, Cluster{}},
		{"minikube", []Node{minikube("fleet-k8s", "fleet-k8s")}, Cluster{Minikube, "fleet-k8s"}},
		{"minikube multi-node", []Node{minikube("dev", "dev"), minikube("dev", "dev-m02")}, Cluster{Minikube, "dev"}},
		{"kind", []Node{kind("kind", "kind-control-plane"), kind("kind", "kind-worker")}, Cluster{KinD, "kind"}},
		{"kind with dashes", []Node{kind("my-dev", "my-dev-control-plane")}, Cluster{KinD, "my-dev"}},
		{"kind malformed providerID", []Node{{Name: "x", ProviderID: "kind://docker"}}, Cluster{}},
		{"k3d", []Node{{Name: "k3d-dev-server-0", ProviderID: "k3s://k3d-dev-server-0"}, {Name: "k3d-dev-agent-1"}}, Cluster{K3d, "dev"}},
		{"k3d dashed name", []Node{{Name: "k3d-my-team-server-0"}}, Cluster{K3d, "my-team"}},
		{"docker desktop", []Node{{Name: "docker-desktop"}}, Cluster{DockerDesktop, "docker-desktop"}},
		{"cloud node", []Node{{Name: "gke-pool-1-abc", ProviderID: "gce://proj/zone/gke-pool-1-abc"}}, Cluster{}},
		{"mixed minikube and cloud", []Node{minikube("dev", "dev"), {Name: "ip-10-0-0-1"}}, Cluster{}},
		{"two minikube profiles", []Node{minikube("a", "a"), minikube("b", "b")}, Cluster{}},
		{"k3d prefix without role", []Node{{Name: "k3d-dev"}}, Cluster{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(tt.nodes); got != tt.want {
				t.Fatalf("Detect = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClusterString(t *testing.T) {
	if s := (Cluster{}).String(); s != "" {
		t.Fatalf("zero String = %q", s)
	}
	if (Cluster{}).Local() {
		t.Fatal("zero Cluster is local")
	}
	if s := (Cluster{Minikube, "p"}).String(); s != "minikube:p" {
		t.Fatalf("String = %q", s)
	}
}
