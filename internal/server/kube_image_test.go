//go:build !nok8s

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_image_test.go drives image delivery as the connections see it: a
// gateway image server whose public side is an in-process TLS stand-in for
// the gateway (it strips the registry id, as the gateway does), so the
// daemon-side manifest check runs over real HTTPS.

// fakeRegistryTunnel is the tunnel's image registry state.
type fakeRegistryTunnel struct {
	mu          sync.Mutex
	prefix, why string
}

func (f *fakeRegistryTunnel) Registry() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefix, f.why
}

func (*fakeRegistryTunnel) WarmRegistry() {}

func (f *fakeRegistryTunnel) set(prefix, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefix, f.why = prefix, why
}

const testRID = "1111111111111111111111111111111111111111111111111111111111111111"

// withGatewayImages gives the fixture's daemon image delivery with a gateway
// image server behind an HTTPS stand-in for the gateway's public side.
func withGatewayImages(t *testing.T, f *kubeFixture) (*fakeRegistryTunnel, *kubeImages) {
	t.Helper()
	m := f.svc.kube
	im := &kubeImages{manager: &kubeimage.Manager{IndexPath: state.KubeImagesPath}, uplink: &imageserve.Uplink{}}
	gw := &imageserve.Gateway{Store: &imageserve.Store{Dir: state.KubeImageCacheDir()}, Clusters: &imageserve.StateClusters{}, Uplink: im.uplink, Index: im.manager, OnPull: m.pullProgress}
	public := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/v2/"+testRID+"/")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"NAME_UNKNOWN"}]}`)
			return
		}
		r.URL.Path = "/v2/" + rest
		gw.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(public.Close)
	gw.Transport = public.Client().Transport
	tunnel := &fakeRegistryTunnel{prefix: strings.TrimPrefix(public.URL, "https://") + "/" + testRID}
	gw.Tunnel = tunnel
	im.gateway = gw
	im.manager.Gateway = gw
	m.mu.Lock()
	m.images = im
	m.mu.Unlock()
	return tunnel, im
}

func kubeCheckOf(t *testing.T, st *fleetgrpc.KubeClusterStatus, name string) *fleetgrpc.KubeCheck {
	t.Helper()
	for _, c := range st.GetChecks() {
		if c.GetName() == name {
			return c
		}
	}
	t.Fatalf("no %s check in %v", name, st.GetChecks())
	return nil
}

// A connection whose delivery is Auto on a cloud cluster without a
// registry delivers through the gateway: the full preflight fetches the
// probe image's manifest through the gateway's public URL with the
// connection's credential, which is made on the record and never leaves the
// daemon. With the gateway's registry gone the connection is LIMITED, and
// says why Gateway cannot be picked.
func TestKubeGatewayDelivery(t *testing.T) {
	f := newKubeFixture(t)
	tunnel, im := withGatewayImages(t, f)
	id := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	host, _, _ := strings.Cut(tunnel.prefix, "/")

	st, err := f.svc.kube.test(context.Background(), id, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := kubeCheckOf(t, st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || c.GetDetail() != "gateway: route + credential ok ("+host+")" {
		t.Fatalf("image delivery check %v", c)
	}
	del := st.GetCapabilities().GetImageDelivery()
	if del.GetMode() != "gateway" || !del.GetGatewayManifestOk() || !del.GetGatewayRegistry() || del.GetGatewayReason() != "" || del.GetGatewayHost() != host ||
		del.GetUploadPacePercent() != 80 || del.GetCacheBytes() == 0 || !strings.Contains(del.GetReason(), "no registry set") {
		t.Fatalf("image delivery %v", del)
	}
	if strings.Contains(st.GetMessage(), "deliver") || strings.Contains(st.GetMessage(), "gateway") {
		t.Fatalf("a working gateway made the connection %v %q", st.GetState(), st.GetMessage())
	}
	cred := f.stored(id).Image.GatewayCredential
	if cred == "" {
		t.Fatal("the connection got no gateway credential")
	}
	list, _ := f.svc.ListKubeClusters(context.Background(), &fleetgrpc.ListKubeClustersRequest{})
	for _, msg := range []proto.Message{st, list} {
		b, _ := proto.Marshal(msg)
		if bytes.Contains(b, []byte(cred)) {
			t.Fatalf("a %T carries the gateway credential", msg)
		}
	}
	if prefix, _ := im.gateway.Store.Prefix(id); prefix != tunnel.prefix {
		t.Fatalf("recorded prefix %q", prefix)
	}

	// An unsaved connection checks the route without a credential.
	draft := f.svc.kube.testDraft(context.Background(), kubeRecordFromProto(&fleetgrpc.KubeCluster{Context: "prod", Namespace: "team", KubeconfigPaths: []string{f.kubeconfig}}), nil)
	if c := kubeCheckOf(t, draft, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || !strings.Contains(c.GetDetail(), "route ok") {
		t.Fatalf("a draft's image delivery %v", c)
	}

	// The registry id is not routed (the gateway forgot the session): the
	// manifest check names the route.
	tunnel.set(host+"/"+strings.Repeat("2", 64), "")
	st, _ = f.svc.kube.test(context.Background(), id, false, nil)
	if c := kubeCheckOf(t, st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(c.GetDetail(), "gateway route: ") {
		t.Fatalf("an unrouted registry id %v", c)
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED || !strings.Contains(st.GetMessage(), "gateway delivery failing (route)") || st.GetCapabilities().GetImageDelivery().GetGatewayFailedStep() != "route" {
		t.Fatalf("an unrouted registry id: %v %q", st.GetState(), st.GetMessage())
	}

	// Enable Image Registry off: Auto finds nothing, and Gateway says why.
	tunnel.set("", "Enable Image Registry is off")
	st, _ = f.svc.kube.test(context.Background(), id, false, nil)
	del = st.GetCapabilities().GetImageDelivery()
	if del.GetMode() != "none" || del.GetGatewayRegistry() || del.GetGatewayReason() != "Enable Image Registry is off" {
		t.Fatalf("no registry: %v", del)
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED || !strings.Contains(st.GetMessage(), "built images cannot be delivered") {
		t.Fatalf("no registry: %v %q", st.GetState(), st.GetMessage())
	}
	// The availability is live: a connection's status follows the tunnel
	// before any probe.
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)
	tunnel.set(host+"/"+testRID, "")
	if del := w.status().GetCapabilities().GetImageDelivery(); !del.GetGatewayRegistry() || del.GetGatewayReason() != "" || del.GetGatewayHost() != host {
		t.Fatalf("live gateway availability %v", del)
	}
}

// awaitFull waits for a running connection's first full preflight.
func (f *kubeFixture) awaitFull(id string) *kubeWorker {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if w := f.svc.kube.worker(id); w != nil {
			w.mu.Lock()
			done := w.haveFull
			w.mu.Unlock()
			if done {
				return w
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("connection %s never finished a full preflight", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A registry set takes Auto's pick ahead of the gateway; one the daemon host
// cannot reach is LIMITED. Node load on a local cluster comes first.
func TestKubeDeliveryOrder(t *testing.T) {
	f := newKubeFixture(t)
	withGatewayImages(t, f)
	reg := f.add(&fleetgrpc.KubeCluster{Context: "prod", Image: &fleetgrpc.KubeImageSettings{Registry: "127.0.0.1:1/team", InsecureRegistry: true}}).GetCluster().GetId()
	st, _ := f.svc.kube.test(context.Background(), reg, false, nil)
	if del := st.GetCapabilities().GetImageDelivery(); del.GetMode() != "registry" || del.GetRegistryReachable() {
		t.Fatalf("an unreachable registry: %v", del)
	}
	if c := kubeCheckOf(t, st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.Contains(st.GetMessage(), "image registry unreachable") {
		t.Fatalf("an unreachable registry: %v %q", c, st.GetMessage())
	}

	// minikube with its tool on the connection's extra PATH: node load.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "minikube"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := f.add(&fleetgrpc.KubeCluster{Context: "minikube", ExtraPath: []string{bin}}).GetCluster().GetId()
	st, _ = f.svc.kube.test(context.Background(), local, false, nil)
	if c := kubeCheckOf(t, st, preflight.CheckImageDelivery); st.GetCapabilities().GetImageDelivery().GetMode() != "node-load" || c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || !strings.HasPrefix(c.GetDetail(), "node load: local cluster (minikube:minikube)") {
		t.Fatalf("a local cluster: %v %v", st.GetCapabilities().GetImageDelivery(), c)
	}
}

// The deep test has a pod pull the probe image through the gateway, with
// the connection's gateway pull Secret applied server-side in its
// namespace; the pod goes afterwards and the status records the deep test.
func TestKubeDeepTestPullsThroughTheGateway(t *testing.T) {
	f := newKubeFixture(t)
	withGatewayImages(t, f)
	id := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	f.start()
	f.awaitFull(id)
	cs := f.apis["prod"]
	var pulled []string
	var mu sync.Mutex
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		if !strings.HasPrefix(name, "fleet-probe-pull-") {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), "team", name)
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		mu.Lock()
		pulled = append(pulled, pod.Spec.Containers[0].Image)
		mu.Unlock()
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "pull", ImageID: pod.Spec.Containers[0].Image,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "StartError", ExitCode: 128}}}}
		return true, pod, nil
	})

	st, err := f.svc.kube.test(context.Background(), id, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := kubeCheckOf(t, st, preflight.CheckImagePull); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
		t.Fatalf("the deep pull %v", c)
	}
	if st.GetDeepCheckedAt() == nil {
		t.Fatal("deep_checked_at not set")
	}
	if len(pulled) == 0 || !strings.Contains(pulled[0], "/"+testRID+"/devcontainer@sha256:") {
		t.Fatalf("pulled %v", pulled)
	}
	secretName := imageserve.GatewayPullSecretName(testIdentity.DaemonID, id)
	secret, err := cs.CoreV1().Secrets("team").Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the gateway pull Secret: %v", err)
	}
	if secret.Labels[names.LabelRole] != string(names.RolePullSecret) || secret.Labels[names.LabelCluster] != id || secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("pull Secret %v %s", secret.Labels, secret.Type)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password string } `json:"auths"`
	}
	_ = json.Unmarshal(secret.Data[".dockerconfigjson"], &cfg)
	for _, a := range cfg.Auths {
		if a.Username != id || a.Password != f.stored(id).Image.GatewayCredential {
			t.Fatalf("pull Secret login %q", a.Username)
		}
	}
	pods, _ := cs.CoreV1().Pods("team").List(context.Background(), metav1.ListOptions{})
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "fleet-probe-pull-") {
			t.Fatalf("the pull pod %s was left behind", p.Name)
		}
	}
	// A later full run without the deep test keeps when it last ran.
	st, _ = f.svc.kube.test(context.Background(), id, false, nil)
	if st.GetDeepCheckedAt() == nil {
		t.Fatal("a full run forgot the deep test")
	}

	// Removing the connection takes its pull Secret and its gateway images.
	if _, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	probe, _ := imageserve.ProbeDigest("linux/amd64")
	if f.svc.kube.images.gateway.Store.Serves(id, probe) {
		t.Fatal("a removed connection is still served the probe image")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := cs.CoreV1().Secrets("team").Get(context.Background(), secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removed connection's pull Secret stays: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The tunnel's registry coming or going re-runs the full preflight of the
// connections that can deliver through the gateway, and only those.
func TestKubeRegistryChangeRejudgesGatewayConnections(t *testing.T) {
	defer func(d time.Duration) { kubeRegistrySettle = d }(kubeRegistrySettle)
	kubeRegistrySettle = 10 * time.Millisecond
	f := newKubeFixture(t)
	withGatewayImages(t, f)
	gw := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	reg := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Mode: "registry", Registry: "registry.example.com"}}).GetCluster().GetId()
	f.start()
	f.awaitFull(gw)
	f.awaitFull(reg)
	lastFull := func(id string) time.Time {
		w := f.svc.kube.worker(id)
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lastFull
	}
	before := map[string]time.Time{gw: lastFull(gw), reg: lastFull(reg)}
	f.svc.kube.registryStateChanged("", "the gateway tunnel is connecting")
	deadline := time.Now().Add(5 * time.Second)
	for !lastFull(gw).After(before[gw]) {
		if time.Now().After(deadline) {
			t.Fatal("a gateway connection was not judged again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastFull(reg) != before[reg] {
		t.Fatal("a registry connection was judged again")
	}
}

// A connection's status carries its last pull through the gateway (live),
// and the push follows the pull.
func TestKubeStatusShowsTheLastGatewayPull(t *testing.T) {
	defer func(d time.Duration) { kubePullPushDelay = d }(kubePullPushDelay)
	kubePullPushDelay = time.Millisecond
	f := newKubeFixture(t)
	tunnel, im := withGatewayImages(t, f)
	id := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	f.start()
	f.awaitFull(id)
	sub := newSubscriber(false)
	f.svc.hub.post(func(h *hub) { h.addSub(sub) })
	// An image delivered to the connection (a docker save archive, as
	// Deliver would import), pulled the way a node does.
	archive, imageID := testImageArchive(t)
	prep, err := im.gateway.Store.Import(context.Background(), bytes.NewReader(archive), imageID, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := im.gateway.Store.Ref(id, prep.Digest, imageID); err != nil {
		t.Fatal(err)
	}
	host, _, _ := strings.Cut(tunnel.prefix, "/")
	cred := f.stored(id).Image.GatewayCredential
	client := &http.Client{Transport: im.gateway.Transport}
	get := func(path string) {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/v2/"+testRID+"/devcontainer/"+path, nil)
		req.SetBasicAuth(id, cred)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get("manifests/" + prep.Digest)
	var m struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	_ = json.Unmarshal(blobOf(t, im, prep.Digest), &m)
	get("blobs/" + m.Layers[0].Digest)
	pull := f.svc.kube.status(id).GetCapabilities().GetImageDelivery().GetLastPull()
	if pull.GetManifest() != prep.Digest || pull.GetBytes() == 0 || pull.GetStartedAt() == nil || pull.GetLastByteAt() == nil {
		t.Fatalf("last pull %v", pull)
	}
	// The pull was pushed to watchers without waiting for a probe.
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, c := range sub.takeKubeClusters().GetClusters() {
			if c.GetCluster().GetId() == id && c.GetCapabilities().GetImageDelivery().GetLastPull().GetBytes() == pull.GetBytes() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the pull was never pushed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func blobOf(t *testing.T, im *kubeImages, digest string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(im.gateway.Store.Dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testImageArchive is a one-layer linux/amd64 image saved as an OCI layout.
func testImageArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	digest := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	body := bytes.Repeat([]byte("fleet "), 4096)
	_ = tw.WriteHeader(&tar.Header{Name: "app", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(layer.Bytes())
	_ = zw.Close()
	cfg, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digest(layer.Bytes())}}})
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(cfg), "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digest(gz.Bytes()), "size": gz.Len()}}})
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest(man), "size": len(man)}}})
	var out bytes.Buffer
	aw := tar.NewWriter(&out)
	for name, b := range map[string][]byte{"index.json": idx, "blobs/sha256/" + digest(cfg)[7:]: cfg, "blobs/sha256/" + digest(gz.Bytes())[7:]: gz.Bytes(), "blobs/sha256/" + digest(man)[7:]: man} {
		_ = aw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = aw.Write(b)
	}
	_ = aw.Close()
	return out.Bytes(), digest(cfg)
}
