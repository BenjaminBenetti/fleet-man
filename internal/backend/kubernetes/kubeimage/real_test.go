package kubeimage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

// Tests against a real docker and registry:2 (FLEET_KUBEIMAGE_DOCKER=1) and
// against a real minikube profile (FLEET_KUBEIMAGE_MINIKUBE=<profile>). Both
// clean up what they create: containers, images and tags, and their
// fleet-it-p2-06-* namespaces.

// realDaemonID is a fresh daemon id per run, so runs never share a
// repository path.
func realDaemonID(t *testing.T) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runQuiet(name string, args ...string) {
	_ = exec.Command(name, args...).Run()
}

// buildUniqueImage builds a tiny image no kubelet has ever seen (a random
// file on busybox) and returns its tag and ID; the image goes at cleanup.
func buildUniqueImage(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	// Truly unique: a kubelet that once pulled an image ID under another
	// name refuses a Never pull of it (KubeletEnsureSecretPulledImages).
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:musl\nRUN echo "+nonce+" > /fleet-nonce\nCMD [\"sleep\", \"3600\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "fleet-it-p2-06:" + nonce
	run(t, "docker", "build", "-q", "-t", tag, dir)
	id := run(t, "docker", "image", "inspect", "--format", "{{.Id}}", tag)
	t.Cleanup(func() { runQuiet("docker", "image", "rm", "-f", tag) })
	return tag, id
}

// startRegistry runs registry:2 on a free loopback port and returns its
// host:port.
func startRegistry(t *testing.T, deleteEnabled bool, htpasswd string) string {
	t.Helper()
	name := "fleet-it-p2-06-reg-" + hex.EncodeToString([]byte(time.Now().Format("150405.000000")))[:12]
	args := []string{"run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::5000", "-e", fmt.Sprintf("REGISTRY_STORAGE_DELETE_ENABLED=%t", deleteEnabled)}
	if htpasswd != "" {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(htpasswd), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-v", dir+":/auth:ro", "-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=fleet", "-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd")
	}
	run(t, "docker", append(args, "registry:2")...)
	t.Cleanup(func() { runQuiet("docker", "rm", "-f", name) })
	port := strings.TrimPrefix(run(t, "docker", "port", name, "5000"), "127.0.0.1:")
	port = strings.Split(port, "\n")[0]
	host := "localhost:" + port
	deadline := time.Now().Add(20 * time.Second)
	for {
		c := newRegistryClient(host, false, Credential{}, nil)
		if c.ping(context.Background()) == nil {
			return host
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry %s never answered", host)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func htpasswdLine(t *testing.T, user, pass string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	return user + ":" + string(h) + "\n"
}

// dockerConfigFor points docker and the test's credential reads at a config
// holding one login.
func dockerConfigFor(t *testing.T, host, user, pass string) {
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, basicAuth(user, pass))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

func TestRealDockerRegistryDelivery(t *testing.T) {
	if os.Getenv("FLEET_KUBEIMAGE_DOCKER") != "1" {
		t.Skip("set FLEET_KUBEIMAGE_DOCKER=1 to run against the host's docker and registry:2")
	}
	ctx := context.Background()
	host := startRegistry(t, true, htpasswdLine(t, "pusher", "pw"))
	dockerConfigFor(t, host, "pusher", "pw")
	_, id := buildUniqueImage(t)
	daemonID := realDaemonID(t)
	secrets := &fakeSecrets{}
	indexDir := t.TempDir()
	m := &Manager{IndexPath: func(c string) string { return filepath.Join(indexDir, c, "images.json") }}
	cluster := &fleet.KubeCluster{ID: "real-docker", Image: fleet.KubeImageSettings{Registry: host + "/team", Retention: "1h"}}
	tgt := Target{Cluster: cluster, DaemonID: daemonID, Platform: "linux/amd64", Secrets: secrets}
	t.Cleanup(func() {
		runQuiet("docker", "image", "rm", RegistryTag(Registry{Domain: host, Prefix: "team"}, daemonID, id))
	})

	d, err := m.Deliver(ctx, tgt, builtImage(id), "inst")
	if err != nil {
		t.Fatal(err)
	}
	repo := RegistryRepository(Registry{Domain: host, Prefix: "team"}, daemonID)
	if d.Strategy != StrategyRegistry || !strings.HasPrefix(d.Ref, repo+"@sha256:") {
		t.Fatalf("Deliver = %+v", d)
	}
	// The digest the pod pulls is the one the registry serves for the tag.
	c := newRegistryClient(host, false, Credential{Username: "pusher", Password: "pw"}, nil)
	_, path, _ := strings.Cut(repo, "/")
	if got, err := c.headManifest(ctx, path, id12(id)); err != nil || got != d.Digest {
		t.Fatalf("registry digest %q (%v), delivery %q", got, err, d.Digest)
	}
	s, ok := secrets.get(PullSecretName(daemonID, cluster.ID))
	if !ok || !strings.Contains(string(s.DockerConfigJSON), host) || strings.Count(string(s.DockerConfigJSON), `"auth"`) != 1 {
		t.Errorf("pull secret %+v", s)
	}
	check, err := m.CheckRegistry(ctx, tgt)
	if err != nil || !check.Reachable || !check.DeleteSupported {
		t.Fatalf("CheckRegistry = %+v, %v", check, err)
	}
	// Read back from the registry as a pod's node would see it.
	img, err := RemoteImageConfig(ctx, d.Ref, "linux/amd64", RemoteOptions{})
	if err != nil || len(img.RepoDigests) != 1 || !strings.HasSuffix(img.RepoDigests[0], d.Digest) {
		t.Errorf("RemoteImageConfig = %+v, %v", img, err)
	}

	if err := m.Release(cluster.ID, "inst"); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	res, err := m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if _, err := c.headManifest(ctx, path, d.Digest); !isNotFound(err) {
		t.Errorf("after GC the manifest answers %v", err)
	}
	if out, _ := exec.Command("docker", "image", "inspect", d.Tag).CombinedOutput(); !strings.Contains(strings.ToLower(string(out)), "no such image") {
		t.Errorf("the host tag %s survived the GC: %s", d.Tag, out)
	}

	// A registry without DELETE keeps the image and says why.
	host2 := startRegistry(t, false, "")
	cluster2 := &fleet.KubeCluster{ID: "real-docker-nodel", Image: fleet.KubeImageSettings{Registry: host2, Retention: "1h"}}
	tgt2 := Target{Cluster: cluster2, DaemonID: daemonID, Platform: "linux/amd64", Secrets: secrets}
	m.Now = nil
	d2, err := m.Deliver(ctx, tgt2, builtImage(id), "inst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runQuiet("docker", "image", "rm", d2.Tag) })
	check, _ = m.CheckRegistry(ctx, tgt2)
	if !check.Reachable || check.DeleteSupported || !strings.Contains(check.Reason, "405") {
		t.Errorf("CheckRegistry = %+v", check)
	}
	_ = m.Release(cluster2.ID, "inst")
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	res, err = m.GC(ctx, tgt2, GCOptions{})
	if err != nil || len(res.Kept) != 1 || len(res.Deleted) != 0 {
		t.Errorf("GC = %+v, %v", res, err)
	}
}

// --- minikube ---

type kubectl struct {
	context, namespace string
}

func (k kubectl) cmd(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", k.context, "-n", k.namespace}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// ApplyPullSecret applies the manifest server-side, as the cluster backend
// does.
func (k kubectl) ApplyPullSecret(ctx context.Context, s PullSecret) error {
	b, err := s.Manifest()
	if err != nil {
		return err
	}
	_, err = k.cmd(ctx, b, "apply", "--server-side", "--field-manager=fleet", "-f", "-")
	return err
}

func (k kubectl) DeletePullSecret(ctx context.Context, name string) error {
	_, err := k.cmd(ctx, nil, "delete", "secret", name, "--ignore-not-found")
	return err
}

func (k kubectl) apply(t *testing.T, manifest string) {
	t.Helper()
	if _, err := k.cmd(context.Background(), []byte(manifest), "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
}

type podState struct {
	Phase   string
	Waiting struct{ Reason, Message string }
	Running bool
	ImageID string
}

func (k kubectl) pod(t *testing.T, name string) podState {
	t.Helper()
	out, err := k.cmd(context.Background(), nil, "get", "pod", name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				ImageID string `json:"imageID"`
				State   struct {
					Waiting *struct{ Reason, Message string } `json:"waiting"`
					Running *struct{}                         `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	st := podState{Phase: p.Status.Phase}
	if len(p.Status.ContainerStatuses) > 0 {
		cs := p.Status.ContainerStatuses[0]
		st.ImageID = cs.ImageID
		st.Running = cs.State.Running != nil
		if cs.State.Waiting != nil {
			st.Waiting = *cs.State.Waiting
		}
	}
	return st
}

// waitPod waits until done says the pod is where the test wants it.
func (k kubectl) waitPod(t *testing.T, name string, timeout time.Duration, done func(podState) bool) podState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := k.pod(t, name)
		if done(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s: still %+v after %v", name, st, timeout)
		}
		time.Sleep(time.Second)
	}
}

func podManifest(name, image, policy string, pullSecrets ...string) string {
	var secrets string
	for _, s := range pullSecrets {
		secrets += fmt.Sprintf("\n  - name: %s", s)
	}
	if secrets != "" {
		secrets = "\n  imagePullSecrets:" + secrets
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  terminationGracePeriodSeconds: 0%s
  containers:
  - name: c
    image: %s
    imagePullPolicy: %s
    command: [sleep, "3600"]
`, name, secrets, image, policy)
}

// minikubeEnv sets up a namespace on the profile's cluster and returns the
// kubectl for it and the detected local cluster.
func minikubeEnv(t *testing.T) (kubectl, localcluster.Cluster, string) {
	t.Helper()
	profile := os.Getenv("FLEET_KUBEIMAGE_MINIKUBE")
	if profile == "" {
		t.Skip("set FLEET_KUBEIMAGE_MINIKUBE=<minikube profile> to run against a real cluster")
	}
	for _, tool := range []string{"docker", "kubectl", "minikube"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is needed: %v", tool, err)
		}
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	k := kubectl{context: profile, namespace: "fleet-it-p2-06-image-strategies-" + hex.EncodeToString(b)}
	run(t, "kubectl", "--context", profile, "create", "namespace", k.namespace)
	t.Cleanup(func() { runQuiet("kubectl", "--context", profile, "delete", "namespace", k.namespace, "--wait=false") })

	out := run(t, "kubectl", "--context", profile, "get", "nodes", "-o", "json")
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		t.Fatal(err)
	}
	var facts []localcluster.Node
	for _, n := range nodes.Items {
		facts = append(facts, localcluster.Node{Name: n.Metadata.Name, Labels: n.Metadata.Labels, ProviderID: n.Spec.ProviderID})
	}
	local := localcluster.Detect(facts)
	if local != (localcluster.Cluster{Kind: localcluster.Minikube, Name: profile}) {
		t.Fatalf("Detect = %+v, want minikube:%s", local, profile)
	}
	return k, local, profile
}

// TestMinikubeNodeLoadAndRedelivery: a built image loaded into minikube runs
// with pull policy Never; once the node loses it the pod waits in
// ErrImageNeverPull, and a re-delivery brings it up; the GC removes it from
// the node and the host.
func TestMinikubeNodeLoadAndRedelivery(t *testing.T) {
	k, local, profile := minikubeEnv(t)
	ctx := context.Background()
	_, id := buildUniqueImage(t)
	daemonID := realDaemonID(t)
	indexDir := t.TempDir()
	m := &Manager{IndexPath: func(c string) string { return filepath.Join(indexDir, c, "images.json") }}
	cluster := &fleet.KubeCluster{ID: "p206-minikube", Image: fleet.KubeImageSettings{Retention: "0"}}
	tgt := Target{Cluster: cluster, DaemonID: daemonID, Local: local, Platform: "linux/amd64", Secrets: k}
	tag := NodeLoadTag(daemonID, id)
	t.Cleanup(func() {
		runQuiet("minikube", "-p", profile, "image", "rm", tag)
		runQuiet("docker", "image", "rm", tag)
	})

	if res := m.Resolve(tgt); res.Strategy != StrategyNodeLoad {
		t.Fatalf("Resolve = %+v", res)
	}
	d, err := m.Deliver(ctx, tgt, builtImage(id), "inst")
	if err != nil {
		t.Fatal(err)
	}
	if d.Ref != tag || d.PullPolicy != PullNever {
		t.Fatalf("Deliver = %+v", d)
	}
	k.apply(t, podManifest("first", d.Ref, d.PullPolicy))
	st := k.waitPod(t, "first", 90*time.Second, func(s podState) bool { return s.Running })
	t.Logf("node-loaded pod running, imageID %s", st.ImageID)

	// The node loses the image (a new node, the kubelet's image GC).
	if _, err := k.cmd(ctx, nil, "delete", "pod", "first", "--wait=true"); err != nil {
		t.Fatal(err)
	}
	run(t, "minikube", "-p", profile, "image", "rm", tag)
	k.apply(t, podManifest("second", d.Ref, d.PullPolicy))
	st = k.waitPod(t, "second", 60*time.Second, func(s podState) bool { return s.Waiting.Reason != "" && s.Waiting.Reason != "ContainerCreating" })
	if !NeedsRedelivery(d.Strategy, st.Waiting.Reason, st.Waiting.Message) {
		t.Fatalf("pod waits with %+v, which needs no re-delivery", st.Waiting)
	}
	t.Logf("after the node lost the image: %s: %s", st.Waiting.Reason, st.Waiting.Message)
	if _, err := m.Redeliver(ctx, tgt, *d); err != nil {
		t.Fatal(err)
	}
	k.waitPod(t, "second", 3*time.Minute, func(s podState) bool { return s.Running })

	// Collected: the node and the host lose fleet's tag.
	if _, err := k.cmd(ctx, nil, "delete", "pod", "second", "--wait=true"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(cluster.ID, "inst"); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Now().Add(time.Hour) }
	res, err := m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if out := run(t, "minikube", "-p", profile, "image", "ls"); strings.Contains(out, strings.TrimPrefix(tag, nodeLoadDomain+"/")) {
		t.Errorf("the node still has %s", tag)
	}
	if out, _ := exec.Command("docker", "image", "inspect", tag).CombinedOutput(); !strings.Contains(strings.ToLower(string(out)), "no such image") {
		t.Errorf("the host still has %s", tag)
	}
}

// TestMinikubeRegistryDelivery: an image pushed to a registry that needs a
// login is pulled by its digest with the managed pull Secret, and only with
// it. The registry is a hostNetwork registry:2 on the node's loopback (which
// containerd pulls from over plain HTTP), reached from the daemon host by a
// port-forward, so one localhost:<port> reference works on both sides.
func TestMinikubeRegistryDelivery(t *testing.T) {
	k, _, _ := minikubeEnv(t)
	ctx := context.Background()
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	port := 31000 + int(b[0])<<4 + int(b[1])%16
	htpasswd := htpasswdLine(t, "pusher", "pw")
	k.apply(t, fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: reg-auth
stringData:
  htpasswd: %q
---
apiVersion: v1
kind: Pod
metadata:
  name: reg
spec:
  hostNetwork: true
  terminationGracePeriodSeconds: 0
  containers:
  - name: registry
    image: registry:2
    env:
    - {name: REGISTRY_HTTP_ADDR, value: "127.0.0.1:%d"}
    - {name: REGISTRY_STORAGE_DELETE_ENABLED, value: "true"}
    - {name: REGISTRY_AUTH, value: htpasswd}
    - {name: REGISTRY_AUTH_HTPASSWD_REALM, value: fleet}
    - {name: REGISTRY_AUTH_HTPASSWD_PATH, value: /auth/htpasswd}
    volumeMounts:
    - {name: auth, mountPath: /auth}
  volumes:
  - name: auth
    secret: {secretName: reg-auth}
`, htpasswd, port))
	k.waitPod(t, "reg", 3*time.Minute, func(s podState) bool { return s.Running })
	pf := exec.Command("kubectl", "--context", k.context, "-n", k.namespace, "port-forward", "pod/reg", fmt.Sprintf("%d:%d", port, port))
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Process.Kill(); _ = pf.Wait() })
	host := fmt.Sprintf("localhost:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for newRegistryClient(host, false, Credential{}, nil).ping(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the registry never answered through the port-forward")
		}
		time.Sleep(300 * time.Millisecond)
	}
	dockerConfigFor(t, host, "pusher", "pw")

	_, id := buildUniqueImage(t)
	daemonID := realDaemonID(t)
	indexDir := t.TempDir()
	m := &Manager{IndexPath: func(c string) string { return filepath.Join(indexDir, c, "images.json") }}
	cluster := &fleet.KubeCluster{ID: "p206-registry", Image: fleet.KubeImageSettings{Mode: fleet.KubeImageRegistry, Registry: host, Retention: "1h"}}
	tgt := Target{Cluster: cluster, DaemonID: daemonID, Platform: "linux/amd64", Secrets: k,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "fleet", "fleet.dev/cluster": cluster.ID}}
	t.Cleanup(func() { runQuiet("docker", "image", "rm", RegistryTag(Registry{Domain: host}, daemonID, id)) })

	d, err := m.Deliver(ctx, tgt, builtImage(id), "inst")
	if err != nil {
		t.Fatal(err)
	}
	secret := PullSecretName(daemonID, cluster.ID)
	if d.Strategy != StrategyRegistry || len(d.PullSecrets) != 1 || d.PullSecrets[0] != secret {
		t.Fatalf("Deliver = %+v", d)
	}
	out, err := k.cmd(ctx, nil, "get", "secret", secret, "-o", "jsonpath={.type} {.metadata.labels.fleet\\.dev/cluster}")
	if err != nil || string(out) != "kubernetes.io/dockerconfigjson "+cluster.ID {
		t.Fatalf("managed secret: %s %v", out, err)
	}

	// Without the Secret the node is refused; with it the pod runs.
	k.apply(t, podManifest("nosecret", d.Ref, d.PullPolicy))
	st := k.waitPod(t, "nosecret", 90*time.Second, func(s podState) bool {
		return s.Waiting.Reason == "ErrImagePull" || s.Waiting.Reason == "ImagePullBackOff" || s.Running
	})
	if msg := strings.ToLower(st.Waiting.Message); st.Running || !strings.Contains(msg, "authoriz") && !strings.Contains(msg, "401") {
		t.Fatalf("a pod without the pull Secret: %+v", st)
	}
	t.Logf("without the pull Secret: %s: %s", st.Waiting.Reason, st.Waiting.Message)
	k.apply(t, podManifest("withsecret", d.Ref, d.PullPolicy, d.PullSecrets...))
	st = k.waitPod(t, "withsecret", 2*time.Minute, func(s podState) bool { return s.Running })
	if !strings.HasSuffix(st.ImageID, d.Digest) {
		t.Errorf("the pod runs %s, not %s", st.ImageID, d.Digest)
	}
	t.Logf("with the managed pull Secret the pod runs %s", st.ImageID)

	check, err := m.CheckRegistry(ctx, tgt)
	if err != nil || !check.Reachable || !check.DeleteSupported {
		t.Errorf("CheckRegistry = %+v, %v", check, err)
	}
	_ = m.Release(cluster.ID, "inst")
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	res, err := m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	c := newRegistryClient(host, false, Credential{Username: "pusher", Password: "pw"}, nil)
	if _, err := c.headManifest(ctx, ownPath("", daemonID), d.Digest); !isNotFound(err) {
		t.Errorf("after GC the manifest answers %v", err)
	}
}
