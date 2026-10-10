package imageserve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// randomBytes is n incompressible bytes from seed.
func randomBytes(n int, seed uint64) []byte {
	r := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)})
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

// fakeClusters is Clusters in memory.
type fakeClusters struct {
	mu       sync.Mutex
	m        map[string]Cluster
	ensured  int
	rotated  int
	failNext error
}

func newFakeClusters(cs ...Cluster) *fakeClusters {
	f := &fakeClusters{m: map[string]Cluster{}}
	for _, c := range cs {
		f.m[c.ID] = c
	}
	return f
}

func (f *fakeClusters) Lookup(id string) (Cluster, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.m[id]
	return c, ok
}

func (f *fakeClusters) EnsureCredential(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured++
	c, ok := f.m[id]
	if !ok {
		return "", fmt.Errorf("no Kubernetes connection %q", id)
	}
	if c.Credential == "" {
		c.Credential = newCredential()
		f.m[id] = c
	}
	return c.Credential, nil
}

func (f *fakeClusters) RotateCredential(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated++
	c, ok := f.m[id]
	if !ok {
		return "", fmt.Errorf("no Kubernetes connection %q", id)
	}
	c.Previous, c.PreviousUntil = c.Credential, time.Now().Add(rotationGrace)
	c.Credential = newCredential()
	f.m[id] = c
	return c.Credential, nil
}

func (f *fakeClusters) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
}

// servedImage is an image prepared in a store and served to connection "c1"
// (credential "pass1"); connection "c2" (credential "pass2") serves nothing.
type servedImage struct {
	srv      *httptest.Server
	store    *Store
	handler  *Handler
	clusters *fakeClusters
	img      testImage
	prep     Prepared
}

func newServedImage(t *testing.T) *servedImage {
	t.Helper()
	s := newTestStore(t)
	p, img := importImage(t, s, map[string]string{"big": string(randomBytes(600<<10, 1))})
	if err := s.Ref("c1", p.Digest, "img"); err != nil {
		t.Fatal(err)
	}
	clusters := newFakeClusters(Cluster{ID: "c1", Credential: "pass1"}, Cluster{ID: "c2", Credential: "pass2"})
	h := &Handler{Store: s, Clusters: clusters}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &servedImage{srv: srv, store: s, handler: h, clusters: clusters, img: img, prep: p}
}

// noRedirect is a client that never follows a redirect.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (si *servedImage) do(t *testing.T, method, path, user, pass string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, si.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func ociCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil || len(e.Errors) == 0 {
		t.Fatalf("not an OCI error body: %q", body)
	}
	return e.Errors[0].Code
}

func TestHandlerManifestHeadersOnHeadAndGet(t *testing.T) {
	si := newServedImage(t)
	path := "/v2/devcontainer/manifests/" + si.prep.Digest
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, body := si.do(t, method, path, "c1", "pass1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s manifest: %d %s", method, resp.StatusCode, body)
		}
		h := resp.Header
		if h.Get("Content-Type") != mediaOCIManifest {
			t.Errorf("%s Content-Type %q", method, h.Get("Content-Type"))
		}
		if h.Get("Docker-Content-Digest") != si.prep.Digest {
			t.Errorf("%s Docker-Content-Digest %q", method, h.Get("Docker-Content-Digest"))
		}
		if h.Get("Content-Length") != strconv.Itoa(len(si.img.manifest)) {
			t.Errorf("%s Content-Length %q, want %d", method, h.Get("Content-Length"), len(si.img.manifest))
		}
		if h.Get("Docker-Distribution-API-Version") != "registry/2.0" {
			t.Errorf("%s API version header %q", method, h.Get("Docker-Distribution-API-Version"))
		}
		if method == http.MethodGet && !bytes.Equal(body, si.img.manifest) {
			t.Errorf("GET manifest body differs")
		}
		if method == http.MethodHead && len(body) != 0 {
			t.Errorf("HEAD manifest has a body")
		}
	}
	if p, ok := si.handler.LastPull("c1"); !ok || p.Manifest != si.prep.Digest {
		t.Errorf("LastPull = %+v, %v", p, ok)
	}
}

func TestHandlerBlobHeadersAndRange(t *testing.T) {
	si := newServedImage(t)
	layer := si.img.layers[0]
	d := sha256Digest(layer)
	path := "/v2/devcontainer/blobs/" + d
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, body := si.do(t, method, path, "c1", "pass1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s blob: %d", method, resp.StatusCode)
		}
		if resp.Header.Get("Docker-Content-Digest") != d || resp.Header.Get("Content-Length") != strconv.Itoa(len(layer)) || resp.Header.Get("Content-Type") != mediaOctetStream {
			t.Errorf("%s blob headers %v", method, resp.Header)
		}
		if resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Errorf("%s blob does not advertise ranges", method)
		}
		if method == http.MethodGet && !bytes.Equal(body, layer) {
			t.Error("GET blob body differs")
		}
	}

	// An interrupted pull resumes where it stopped.
	half := len(layer) / 2
	resp, body := si.do(t, http.MethodGet, path, "c1", "pass1", "Range", fmt.Sprintf("bytes=%d-", half))
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range: %d", resp.StatusCode)
	}
	if want := fmt.Sprintf("bytes %d-%d/%d", half, len(layer)-1, len(layer)); resp.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range %q, want %q", resp.Header.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, layer[half:]) {
		t.Error("the resumed range differs")
	}
	if p, _ := si.handler.LastPull("c1"); p.Bytes != 0 {
		// No manifest GET came first, so no pull is being recorded.
		t.Logf("bytes recorded without a manifest GET: %d", p.Bytes)
	}
	// The config is a blob too.
	if resp, _ := si.do(t, http.MethodGet, "/v2/devcontainer/blobs/"+si.img.configDigest, "c1", "pass1"); resp.StatusCode != http.StatusOK {
		t.Errorf("config blob: %d", resp.StatusCode)
	}
}

func TestHandlerAuth(t *testing.T) {
	si := newServedImage(t)
	path := "/v2/devcontainer/manifests/" + si.prep.Digest
	for name, creds := range map[string][2]string{
		"none":            {"", ""},
		"wrong password":  {"c1", "pass2"},
		"unknown cluster": {"c9", "pass1"},
		"empty password":  {"c1", ""},
	} {
		for _, p := range []string{path, "/v2/"} {
			resp, body := si.do(t, http.MethodGet, p, creds[0], creds[1])
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s: %d", name, p, resp.StatusCode)
				continue
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != `Basic realm="fleet"` {
				t.Errorf("%s: challenge %q", name, got)
			}
			if code := ociCode(t, body); code != "UNAUTHORIZED" {
				t.Errorf("%s: code %s", name, code)
			}
		}
	}
	if resp, body := si.do(t, http.MethodGet, "/v2/", "c1", "pass1"); resp.StatusCode != http.StatusOK || string(body) != "{}" {
		t.Errorf("authenticated API root: %d %q", resp.StatusCode, body)
	}

	// A rotated-away credential works for its grace, then not.
	if _, err := si.clusters.RotateCredential("c1"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := si.do(t, http.MethodHead, path, "c1", "pass1"); resp.StatusCode != http.StatusOK {
		t.Errorf("the previous credential within its grace: %d", resp.StatusCode)
	}
	c, _ := si.clusters.Lookup("c1")
	if resp, _ := si.do(t, http.MethodHead, path, "c1", c.Credential); resp.StatusCode != http.StatusOK {
		t.Errorf("the new credential: %d", resp.StatusCode)
	}
	si.clusters.mu.Lock()
	c.PreviousUntil = time.Now().Add(-time.Second)
	si.clusters.m["c1"] = c
	si.clusters.mu.Unlock()
	if resp, _ := si.do(t, http.MethodHead, path, "c1", "pass1"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the previous credential after its grace: %d", resp.StatusCode)
	}

	// A removed connection's credential stops working at once.
	si.clusters.remove("c1")
	if resp, _ := si.do(t, http.MethodHead, path, "c1", c.Credential); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a removed connection: %d", resp.StatusCode)
	}
}

func TestHandlerServesOnlyTheConnectionsImagesByDigest(t *testing.T) {
	si := newServedImage(t)
	layer := sha256Digest(si.img.layers[0])
	unknown := sha256Digest([]byte("nothing"))
	for _, c := range []struct {
		name, method, path, user, pass, code string
		status                               int
	}{
		{"other connection's manifest", "GET", "/v2/devcontainer/manifests/" + si.prep.Digest, "c2", "pass2", "MANIFEST_UNKNOWN", 404},
		{"other connection's blob", "GET", "/v2/devcontainer/blobs/" + layer, "c2", "pass2", "BLOB_UNKNOWN", 404},
		{"unknown manifest", "HEAD", "/v2/devcontainer/manifests/" + unknown, "c1", "pass1", "", 404},
		{"unknown blob", "GET", "/v2/devcontainer/blobs/" + unknown, "c1", "pass1", "BLOB_UNKNOWN", 404},
		{"a manifest is not a blob", "GET", "/v2/devcontainer/blobs/" + si.prep.Digest, "c1", "pass1", "BLOB_UNKNOWN", 404},
		{"tag", "GET", "/v2/devcontainer/manifests/latest", "c1", "pass1", "MANIFEST_UNKNOWN", 404},
		{"bad digest", "GET", "/v2/devcontainer/manifests/sha256:totallywrong", "c1", "pass1", "MANIFEST_UNKNOWN", 404},
		{"other repository", "GET", "/v2/library/busybox/manifests/" + si.prep.Digest, "c1", "pass1", "NAME_UNKNOWN", 404},
		{"tags list", "GET", "/v2/devcontainer/tags/list", "c1", "pass1", "NAME_UNKNOWN", 404},
		{"not a registry path", "GET", "/healthz", "c1", "pass1", "NAME_UNKNOWN", 404},
		// No path cleaning, so nothing ever redirects (a Location the
		// gateway would hand on without the rid).
		{"double slash", "GET", "/v2/devcontainer//manifests/" + si.prep.Digest, "c1", "pass1", "NAME_UNKNOWN", 404},
		{"dot segment", "GET", "/v2/./devcontainer/manifests/" + si.prep.Digest, "c1", "pass1", "NAME_UNKNOWN", 404},
		{"push", "POST", "/v2/devcontainer/blobs/uploads/", "c1", "pass1", "UNSUPPORTED", 405},
		{"put", "PUT", "/v2/devcontainer/manifests/" + si.prep.Digest, "c1", "pass1", "UNSUPPORTED", 405},
		{"delete", "DELETE", "/v2/devcontainer/manifests/" + si.prep.Digest, "c1", "pass1", "UNSUPPORTED", 405},
	} {
		resp, body := si.do(t, c.method, c.path, c.user, c.pass)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d, want %d (Location %q)", c.name, resp.StatusCode, c.status, resp.Header.Get("Location"))
			continue
		}
		if c.code != "" && ociCode(t, body) != c.code {
			t.Errorf("%s: code %s, want %s", c.name, ociCode(t, body), c.code)
		}
		if c.status == 405 && resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: Allow %q", c.name, resp.Header.Get("Allow"))
		}
	}
}

func TestHandlerRecordsPulls(t *testing.T) {
	si := newServedImage(t)
	if _, ok := si.handler.LastPull("c1"); ok {
		t.Fatal("a pull before any")
	}
	si.do(t, http.MethodGet, "/v2/devcontainer/manifests/"+si.prep.Digest, "c1", "pass1")
	si.do(t, http.MethodGet, "/v2/devcontainer/blobs/"+si.img.configDigest, "c1", "pass1")
	si.do(t, http.MethodGet, "/v2/devcontainer/blobs/"+sha256Digest(si.img.layers[0]), "c1", "pass1")
	p, ok := si.handler.LastPull("c1")
	if want := int64(len(si.img.config) + len(si.img.layers[0])); !ok || p.Bytes != want || p.Last.Before(p.At) {
		t.Fatalf("LastPull = %+v, want %d bytes", p, want)
	}
}

func TestHandlerPacesBlobs(t *testing.T) {
	si := newServedImage(t)
	u := &Uplink{}
	u.estimate = 2 << 20 // 2 MiB/s measured
	si.handler.Uplink = u
	si.clusters.m["c1"] = Cluster{ID: "c1", Credential: "pass1", PacePercent: 50}
	layer := si.img.layers[0]
	start := time.Now()
	resp, body := si.do(t, http.MethodGet, "/v2/devcontainer/blobs/"+sha256Digest(layer), "c1", "pass1")
	took := time.Since(start)
	if resp.StatusCode != 200 || !bytes.Equal(body, layer) {
		t.Fatalf("paced blob: %d", resp.StatusCode)
	}
	// 50% of 2 MiB/s is 1 MiB/s, with a 256 KiB burst first.
	want := time.Duration(float64(max(0, len(layer)-paceBurst)) / (1 << 20) * float64(time.Second))
	if took < want*8/10 {
		t.Fatalf("%d bytes took %v, want about %v", len(layer), took, want)
	}
	t.Logf("%d bytes at 50%% of 2 MiB/s took %v (want ~%v)", len(layer), took, want)
}
