package kubeimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// testDaemonID is a daemon id of the real shape (32 lowercase hex digits).
const testDaemonID = "3fa2c91e0b77d4e5a6b7c8d9e0f1a2b3"

// --- fake registry ---

type authMode int

const (
	authNone authMode = iota
	authBasic
	authBearer
)

type deleteMode int

const (
	deleteEnabled deleteMode = iota
	deleteDisabled
	deleteDenied
)

type fakeManifest struct {
	mediaType string
	body      []byte
}

// fakeRegistry is a small distribution registry: manifests by tag and
// digest, blobs, DELETE (enabled, disabled or denied), and no auth, Basic or
// Bearer token auth with one user.
type fakeRegistry struct {
	t      *testing.T
	srv    *httptest.Server
	auth   authMode
	delete deleteMode
	user   string
	pass   string
	// anonymousPull lets requests without a login pull (auth then guards
	// only DELETE).
	anonymousPull bool
	// noHeadDigest leaves Docker-Content-Digest out of HEAD answers.
	noHeadDigest bool

	mu        sync.Mutex
	manifests map[string]map[string]fakeManifest // repo → tag or digest → manifest
	blobs     map[string][]byte
	requests  []string
	scopes    []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	r := &fakeRegistry{t: t, user: "u", pass: "p", manifests: map[string]map[string]fakeManifest{}, blobs: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// host is the test server's host:port.
func (r *fakeRegistry) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

// transport sends every request, whatever its host and scheme, to the fake.
func (r *fakeRegistry) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out := req.Clone(req.Context())
		out.URL.Scheme = "http"
		out.URL.Host = r.host()
		out.Host = r.host()
		return http.DefaultTransport.RoundTrip(out)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// addManifest stores a manifest under repo by its digest and, when tag is
// set, by tag; it returns the digest.
func (r *fakeRegistry) addManifest(repo, tag, mediaType string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := sha256Digest(body)
	if r.manifests[repo] == nil {
		r.manifests[repo] = map[string]fakeManifest{}
	}
	m := fakeManifest{mediaType: mediaType, body: body}
	r.manifests[repo][d] = m
	if tag != "" {
		r.manifests[repo][tag] = m
	}
	return d
}

func (r *fakeRegistry) addBlob(body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := sha256Digest(body)
	r.blobs[d] = body
	return d
}

func (r *fakeRegistry) has(repo, ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.manifests[repo][ref]
	return ok
}

func (r *fakeRegistry) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func (r *fakeRegistry) count(prefix string) int {
	n := 0
	for _, s := range r.seen() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func writeOCIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func (r *fakeRegistry) authorized(req *http.Request) bool {
	switch r.auth {
	case authBasic:
		u, p, ok := req.BasicAuth()
		return ok && u == r.user && p == r.pass
	case authBearer:
		return req.Header.Get("Authorization") == "Bearer good-token"
	}
	return true
}

func (r *fakeRegistry) challenge(w http.ResponseWriter, req *http.Request, scope string) {
	if r.auth == authBasic {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	} else {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="fake",scope="%s"`, req.Host, scope))
	}
	writeOCIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	if req.URL.Path == "/token" {
		u, p, ok := req.BasicAuth()
		r.mu.Lock()
		r.scopes = append(r.scopes, req.URL.Query()["scope"]...)
		r.mu.Unlock()
		if !ok {
			// Anonymous tokens pull only.
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon-token"})
			return
		}
		if u != r.user || p != r.pass {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"details": "incorrect username or password"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "good-token"})
		return
	}
	if req.URL.Path == "/v2/" {
		if r.auth != authNone && !r.authorized(req) {
			r.challenge(w, req, "")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	var repo, kind, ref string
	for _, k := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(rest, k); i >= 0 {
			repo, kind, ref = rest[:i], strings.Trim(k, "/"), rest[i+len(k):]
			break
		}
	}
	if repo == "" {
		http.NotFound(w, req)
		return
	}
	action := "pull"
	if req.Method == http.MethodDelete {
		action = "delete"
	}
	anonOK := r.anonymousPull && action == "pull" && req.Header.Get("Authorization") == "" ||
		r.anonymousPull && action == "pull" && req.Header.Get("Authorization") == "Bearer anon-token"
	if r.auth != authNone && !r.authorized(req) && !anonOK {
		r.challenge(w, req, "repository:"+repo+":"+action)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case "manifests":
		m, found := r.manifests[repo][ref]
		switch req.Method {
		case http.MethodHead, http.MethodGet:
			if !found {
				writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
			w.Header().Set("Content-Type", m.mediaType)
			if req.Method == http.MethodGet || !r.noHeadDigest {
				w.Header().Set("Docker-Content-Digest", sha256Digest(m.body))
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(m.body)))
			if req.Method == http.MethodGet {
				_, _ = w.Write(m.body)
			}
		case http.MethodDelete:
			switch r.delete {
			case deleteDisabled:
				writeOCIError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "The operation is unsupported.")
				return
			case deleteDenied:
				writeOCIError(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
				return
			}
			if !found {
				writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
			for k, v := range r.manifests[repo] {
				if sha256Digest(v.body) == ref {
					delete(r.manifests[repo], k)
				}
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case "blobs":
		b, found := r.blobs[ref]
		if !found {
			writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
			return
		}
		_, _ = w.Write(b)
	}
}

// testImageManifest builds a single-platform image (config + manifest) in
// the registry and returns its manifest digest and config digest.
func (r *fakeRegistry) addImage(repo, tag, arch string, cfg map[string]any) (manifestDigest, configDigest string) {
	full := map[string]any{"architecture": arch, "os": "linux", "config": cfg, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{}}}
	if arch == "arm64" {
		full["variant"] = "v8"
	}
	cb, _ := json.Marshal(full)
	configDigest = r.addBlob(cb)
	mb, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(cb)},
		"layers":        []any{},
	})
	return r.addManifest(repo, tag, mediaOCIManifest, mb), configDigest
}

// addIndex stores an index over the given platform manifests.
func (r *fakeRegistry) addIndex(repo, tag string, platforms map[string]string) string {
	var ms []map[string]any
	for plat, d := range platforms {
		parts := strings.Split(plat, "/")
		p := map[string]any{"os": parts[0], "architecture": parts[1]}
		if len(parts) > 2 {
			p["variant"] = parts[2]
		}
		ms = append(ms, map[string]any{"mediaType": mediaOCIManifest, "digest": d, "size": 1, "platform": p})
	}
	ms = append(ms, map[string]any{"mediaType": mediaOCIManifest, "digest": zeroDigest, "size": 1, "platform": map[string]any{"os": "unknown", "architecture": "unknown"}})
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex, "manifests": ms})
	return r.addManifest(repo, tag, mediaOCIIndex, b)
}

// --- scripted tools ---

// fakeTools are stand-ins for docker and the local cluster tools: shell
// scripts that journal their arguments to <dir>/<tool>.log and answer from
// files in dir.
type fakeTools struct {
	dir string
}

const fakeDockerScript = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/docker.log"
case "$1" in
tag)
	if grep -qxF "$2" "$d/missing" 2>/dev/null; then
		echo "Error response from daemon: No such image: $2" >&2; exit 1
	fi
	echo "$3" >> "$d/tags" ;;
push)
	if [ -f "$d/push_sleep" ]; then sleep "$(cat "$d/push_sleep")"; fi
	if [ -f "$d/push_fail" ]; then cat "$d/push_fail" >&2; exit 1; fi
	echo "The push refers to repository [$2]"
	echo "abc123: Pushed"
	echo "${2##*:}: digest: $(cat "$d/push_digest") size: 528" ;;
image)
	case "$2" in
	inspect)
		for a in "$@"; do ref=$a; done
		if grep -qxF "$ref" "$d/missing" 2>/dev/null; then
			echo "Error response from daemon: No such image: $ref" >&2; exit 1
		fi
		echo "sha256:present" ;;
	rm) ;;
	esac ;;
esac
`

const fakeClusterToolScript = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/$(basename "$0").log"
if [ -f "$d/$(basename "$0").fail" ]; then cat "$d/$(basename "$0").fail" >&2; exit 1; fi
`

func newFakeTools(t *testing.T) *fakeTools {
	t.Helper()
	f := &fakeTools{dir: t.TempDir()}
	f.write(t, "docker", fakeDockerScript, 0o755)
	for _, tool := range []string{"minikube", "kind", "k3d"} {
		f.write(t, tool, fakeClusterToolScript, 0o755)
	}
	return f
}

func (f *fakeTools) write(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeTools) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fakeTools) lines(t *testing.T, name string) []string {
	var out []string
	for _, l := range strings.Split(f.read(t, name), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// --- fake secrets and credentials ---

type fakeSecrets struct {
	mu      sync.Mutex
	applied map[string]PullSecret
	deletes []string
	err     error
}

func (s *fakeSecrets) ApplyPullSecret(_ context.Context, p PullSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.applied == nil {
		s.applied = map[string]PullSecret{}
	}
	s.applied[p.Name] = p
	return nil
}

func (s *fakeSecrets) DeletePullSecret(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.applied, name)
	s.deletes = append(s.deletes, name)
	return nil
}

func (s *fakeSecrets) get(name string) (PullSecret, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.applied[name]
	return p, ok
}

type staticCredentials map[string]Credential

// lookupCount counts every staticCredentials lookup of the test binary.
var lookupCount atomic.Int64

func (c staticCredentials) Lookup(_ context.Context, domain string) (Credential, error) {
	lookupCount.Add(1)
	return c[domain], nil
}

func (c staticCredentials) lookups() int64 { return lookupCount.Load() }

// --- harness ---

type harness struct {
	m       *Manager
	tools   *fakeTools
	reg     *fakeRegistry
	secrets *fakeSecrets
	creds   staticCredentials
	index   string
	clock   *testClock
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{tools: newFakeTools(t), reg: newFakeRegistry(t), secrets: &fakeSecrets{}, creds: staticCredentials{}}
	h.index = filepath.Join(t.TempDir(), "kube")
	h.clock = &testClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	h.m = &Manager{
		Docker:             filepath.Join(h.tools.dir, "docker"),
		IndexPath:          func(id string) string { return filepath.Join(h.index, id, "images.json") },
		Credentials:        h.creds,
		Transport:          h.reg.transport(),
		Now:                h.clock.Now,
		passthroughTimeout: 5 * time.Second,
	}
	return h
}

func (h *harness) cluster(mutate ...func(*fleet.KubeCluster)) *fleet.KubeCluster {
	c := &fleet.KubeCluster{ID: "dev-abc123", DisplayName: "dev", Context: "dev", Namespace: "fleet", ExtraPath: []string{h.tools.dir}}
	for _, f := range mutate {
		f(c)
	}
	return c
}

func (h *harness) target(c *fleet.KubeCluster, local localcluster.Cluster) Target {
	return Target{Cluster: c, DaemonID: testDaemonID, Local: local, Platform: "linux/amd64", Secrets: h.secrets, Labels: map[string]string{"fleet.dev/cluster": c.ID}}
}

func (h *harness) indexOf(t *testing.T, clusterID string) *Index {
	t.Helper()
	ix, err := loadIndex(h.m.IndexPath(clusterID))
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// builtImage is an image the CLI built.
func builtImage(id string) Image {
	return Image{Ref: "vsc-app-1234-features", Config: imageConfig(id), Built: true}
}

func imageConfig(id string) kubeshim.ImageConfig {
	return kubeshim.ImageConfig{ID: id, Architecture: "amd64", Os: "linux"}
}

func basicAuth(u, p string) string { return base64.StdEncoding.EncodeToString([]byte(u + ":" + p)) }

// imageID is a valid image ID made from a short name.
func imageID(name string) string { return sha256Digest([]byte(name)) }
