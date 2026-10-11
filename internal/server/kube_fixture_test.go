//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_fixture_test.go is the kube RPC tests' world: a kubeconfig on the
// "daemon host" (a temp HOME) and fake API servers behind its contexts, so
// the RPCs, the workers and their probes run for real against clusters that
// answer like a fresh minikube.

const (
	fakeClusterUID = "0d3c1f9e-aaaa-bbbb-cccc-000000000001"
	// fakeKubeToken is the static token of the kubeconfig's token-ctx user:
	// no reply may ever carry it.
	fakeKubeToken = "kube-SECRET-token-31337"
)

var testIdentity = owner.Identity{DaemonID: "00112233445566778899aabbccddeeff", Nonce: "aaaaaaaaaaaaaaaa", Host: "laptop"}

// newFakeKubeAPI is a fake API server shaped like a fresh minikube (local)
// or a cloud cluster: one node, the hostpath class, namespaces default and
// fleet, and an identity allowed everything.
func newFakeKubeAPI(t *testing.T, uid string, local bool, objects ...runtime.Object) *fake.Clientset {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gke-pool-1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64"}}}
	if local {
		node.Name, node.Labels = "minikube", map[string]string{"minikube.k8s.io/name": "minikube"}
	}
	base := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}},
		node,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "k8s.io/minikube-hostpath"},
	}
	cs := fake.NewClientset(append(base, objects...)...)
	honorDryRun(cs)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.37.0"}
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: "minikube-user"}}}, nil
	})
	cs.PrependReactor("create", "selfsubjectrulesreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectRulesReview{Status: authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
		}}, nil
	})
	return cs
}

type kubeFixture struct {
	t    *testing.T
	svc  *service
	home string
	// kubeconfig is the daemon host's kubeconfig ($KUBECONFIG in fleetd's
	// environment); apis are the fake clusters behind its contexts.
	kubeconfig string
	apis       map[string]*fake.Clientset
	// real lists contexts that connect through kubeexec for real (exec
	// plugins) instead of a fake API server.
	real map[string]bool
	// pooled is the last set of connections handed to the daemon's pool
	// (configureKubePool, stubbed so the process-wide pool is left alone).
	pooledMu sync.Mutex
	pooled   []fleet.KubeCluster
}

// pooledIDs are the ids of the connections the pool was last handed.
func (f *kubeFixture) pooledIDs() []string {
	f.pooledMu.Lock()
	defer f.pooledMu.Unlock()
	var ids []string
	for _, rec := range f.pooled {
		ids = append(ids, rec.ID)
	}
	return ids
}

// newKubeFixture isolates HOME and builds a service whose kube manager
// reaches fake clusters: minikube and minikube-alias (two contexts to one
// local cluster), prod (a cloud cluster, namespace team) and token-ctx (a
// static token user whose cluster cannot be reached: CONTEXT_MISSING).
func newKubeFixture(t *testing.T) *kubeFixture {
	t.Helper()
	isolateFleetDir(t)
	home := os.Getenv("HOME")
	f := &kubeFixture{t: t, home: home, apis: map[string]*fake.Clientset{}, real: map[string]bool{}}
	f.kubeconfig = writeKubeconfigFile(t, filepath.Join(home, ".kube", "config"), "minikube", fmt.Sprintf(`
- name: minikube
  context: {cluster: minikube, user: minikube}
- name: minikube-alias
  context: {cluster: minikube, user: minikube, namespace: fleet}
- name: prod
  context: {cluster: prod, user: minikube, namespace: team}
- name: token-ctx
  context: {cluster: prod, user: token-user}
`), fmt.Sprintf(`
- name: minikube
  user: {client-certificate-data: %[1]s, client-key-data: %[1]s}
- name: token-user
  user: {token: %[2]s}
`, "Y2VydA==", fakeKubeToken))
	minikube := newFakeKubeAPI(t, fakeClusterUID, true)
	f.apis["minikube"], f.apis["minikube-alias"] = minikube, minikube
	f.apis["prod"] = newFakeKubeAPI(t, "0d3c1f9e-aaaa-bbbb-cccc-000000000002", false, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}})

	f.svc = newService()
	m := f.svc.kube
	m.configurePool = func(records []fleet.KubeCluster) {
		f.pooledMu.Lock()
		f.pooled = append([]fleet.KubeCluster(nil), records...)
		f.pooledMu.Unlock()
	}
	m.environ = func() []string { return []string{"PATH=/usr/bin:/bin", "HOME=" + home, "KUBECONFIG=" + f.kubeconfig} }
	m.loadIdentity = func() (owner.Identity, error) { return testIdentity, nil }
	m.connect = func(rec fleet.KubeCluster, environ []string) (*kubeLink, error) {
		if f.real[rec.Context] {
			return connectKube(rec, environ)
		}
		cs := f.apis[rec.Context]
		if cs == nil {
			return nil, fmt.Errorf("%w: no context %q", kubeexec.ErrContextMissing, rec.Context)
		}
		return &kubeLink{
			client: cs, auth: kubeexec.AuthClientCert, server: "https://" + rec.Context + ".test:8443", source: "~/.kube/config",
			credential: func(context.Context) (kubeexec.Credential, error) {
				return kubeexec.Credential{Auth: kubeexec.AuthClientCert, ExpiresAt: time.Now().Add(365 * 24 * time.Hour)}, nil
			},
		}, nil
	}
	return f
}

// writeKubeconfigFile writes a kubeconfig whose clusters are minikube and
// prod (unreachable addresses; the fakes stand in for them).
func writeKubeconfigFile(t *testing.T, path, current, contexts, users string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
clusters:
- name: minikube
  cluster: {server: "https://127.0.0.1:1"}
- name: prod
  cluster: {server: "https://127.0.0.1:2"}
contexts:%s
users:%s`, current, contexts, users)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// start runs the kube manager (its workers) until the test ends.
func (f *kubeFixture) start() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	go f.svc.hub.run(ctx)
	go f.svc.kube.run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.svc.kube.mu.Lock()
		running := f.svc.kube.ctx != nil
		f.svc.kube.mu.Unlock()
		if running {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the kube manager never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// add adds a connection and fails the test on error.
func (f *kubeFixture) add(c *fleetgrpc.KubeCluster) *fleetgrpc.KubeClusterStatus {
	f.t.Helper()
	reply, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: c})
	if err != nil {
		f.t.Fatalf("AddKubeCluster(%v): %v", c, err)
	}
	return reply.GetCluster()
}

// stored is the record as kube_clusters.json holds it.
func (f *kubeFixture) stored(id string) fleet.KubeCluster {
	f.t.Helper()
	reg, err := state.LoadKubeClusters()
	if err != nil {
		f.t.Fatal(err)
	}
	rec := reg.Find(id)
	if rec == nil {
		f.t.Fatalf("no record %q in %+v", id, reg.Clusters)
	}
	return *rec
}

// awaitState waits for a connection's pushed state.
func (f *kubeFixture) awaitState(id string, want fleetgrpc.KubeClusterState) *fleetgrpc.KubeClusterStatus {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := f.svc.kube.status(id)
		if st.GetState() == want {
			return st
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("connection %s is %v %q, want %v; checks: %v", id, st.GetState(), st.GetMessage(), want, st.GetChecks())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedInstance records an instance of fleet alpha on connection id.
func seedKubeInstance(t *testing.T, fleetName, instance, cluster, key string) {
	t.Helper()
	err := state.Update(func(st *state.State) error {
		f, ok := st.Fleets[fleetName]
		if !ok {
			f = &fleet.Fleet{Name: fleetName, Remote: "git@example.com:" + fleetName + ".git"}
			st.Fleets[fleetName] = f
		}
		f.Instances = append(f.Instances, &fleet.Instance{Name: instance, Status: fleet.StatusStopped, Backend: fleet.BackendKubernetes, KubeCluster: cluster, KubeKey: key})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// honorDryRun makes the fake API server create nothing for a dry run, as
// a real one does (the preflight canaries are dry runs).
func honorDryRun(cs *fake.Clientset) {
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if ca, ok := a.(k8stesting.CreateActionImpl); ok && slices.Contains(ca.GetCreateOptions().DryRun, metav1.DryRunAll) {
			return true, ca.GetObject(), nil
		}
		return false, nil, nil
	})
}

// newLocalServer is the local socket's server: the service with no
// interceptors.
func newLocalServer(svc *service) *grpc.Server {
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	return gs
}
