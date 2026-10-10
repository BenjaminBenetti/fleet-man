package imageserve

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// handler.go is the pull-only OCI distribution API fleetd serves through
// the gateway's /v2/<rid>/... route (the gateway strips the rid):
//
//	GET|HEAD /v2/                                       API root
//	GET|HEAD /v2/devcontainer/manifests/<sha256 digest> a manifest
//	GET|HEAD /v2/devcontainer/blobs/<sha256 digest>     a config or layer
//
// Only digests are served (no tags), and only what the requesting
// connection serves: its credential (Basic auth: the connection's id and its
// gateway pull credential) names the connection, and a digest of another
// connection's image is as unknown as a digest that exists nowhere. Every
// manifest answer, HEAD and GET, carries the manifest's own media type as
// Content-Type, its Content-Length and its Docker-Content-Digest: containerd
// reads a missing or text/plain type as the removed schema1 format, and
// falls back to GET when a HEAD lacks the size or digest. Blobs are served
// with http.ServeContent, so a single-range request resumes an interrupted
// layer. Nothing is ever redirected: the gateway does not rewrite Location,
// so a redirect would come back without the rid.

// repoName is the one repository the server holds.
const repoName = "devcontainer"

// realm is the Basic realm of the challenge, the gateway's too.
const realm = "fleet"

// Handler is the image server's http.Handler.
type Handler struct {
	Store    *Store
	Clusters Clusters
	// Uplink paces blob uploads; nil sends them unpaced.
	Uplink *Uplink

	mu    sync.Mutex
	pulls map[string]*PullStats
}

// PullStats describe a connection's latest pull through the gateway.
type PullStats struct {
	// Manifest is the manifest it fetched, and At when.
	Manifest string
	At       time.Time
	// Bytes are the blob bytes sent since, and Last when the last of them
	// went.
	Bytes int64
	Last  time.Time
}

// LastPull returns a connection's latest pull (false when it has made
// none since the daemon started).
func (h *Handler) LastPull(clusterID string) (PullStats, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pulls[clusterID]
	if !ok {
		return PullStats{}, false
	}
	return *p, true
}

func (h *Handler) notePull(clusterID, manifest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pulls == nil {
		h.pulls = map[string]*PullStats{}
	}
	p := h.pulls[clusterID]
	if p != nil && p.Manifest == manifest && time.Since(p.At) < time.Minute {
		return
	}
	h.pulls[clusterID] = &PullStats{Manifest: manifest, At: time.Now()}
}

func (h *Handler) noteBytes(clusterID string, n int64) {
	if n <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.pulls[clusterID]; p != nil {
		p.Bytes += n
		p.Last = time.Now()
	}
}

// writeError answers with an OCI distribution error body.
func writeError(w http.ResponseWriter, status int, code, message string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "not a registry path")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "the registry is pull-only")
		return
	}
	cluster, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	if rest == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "2")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("{}"))
		}
		return
	}
	name, tail, ok := strings.Cut(rest, "/")
	if !ok || name != repoName {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown repository")
		return
	}
	kind, ref, ok := strings.Cut(tail, "/")
	switch {
	case ok && kind == "manifests":
		h.serveManifest(w, r, cluster, ref)
	case ok && kind == "blobs":
		h.serveBlob(w, r, cluster, ref)
	default:
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown path")
	}
}

// authenticate returns the connection a request's Basic credential names.
func (h *Handler) authenticate(r *http.Request) (Cluster, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" || pass == "" || h.Clusters == nil {
		return Cluster{}, false
	}
	c, ok := h.Clusters.Lookup(user)
	if !ok {
		return Cluster{}, false
	}
	if c.Credential != "" && subtle.ConstantTimeCompare([]byte(pass), []byte(c.Credential)) == 1 {
		return c, true
	}
	if c.Previous != "" && time.Now().Before(c.PreviousUntil) && subtle.ConstantTimeCompare([]byte(pass), []byte(c.Previous)) == 1 {
		return c, true
	}
	return Cluster{}, false
}

func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, c Cluster, ref string) {
	if !validDigest(ref) {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifests are served by digest only")
		return
	}
	m, ok := h.Store.manifestFor(c.ID, ref)
	if !ok {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	f, err := os.Open(h.Store.blobPath(ref))
	if err != nil {
		flog.Warn("gateway image server: a served manifest is missing from the cache", "cluster", c.ID, "digest", ref, "err", err)
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	defer f.Close()
	hd := w.Header()
	hd.Set("Content-Type", m.MediaType)
	hd.Set("Docker-Content-Digest", ref)
	hd.Set("Etag", `"`+ref+`"`)
	// ServeContent sets Content-Length (on HEAD too) from the file.
	if r.Method == http.MethodGet {
		h.notePull(c.ID, ref)
		flog.Info("gateway image server: manifest pulled", "cluster", c.ID, "digest", ref)
	}
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, c Cluster, ref string) {
	if !validDigest(ref) || !h.Store.servesBlob(c.ID, ref) {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	f, err := os.Open(h.Store.blobPath(ref))
	if err != nil {
		flog.Warn("gateway image server: a served blob is missing from the cache", "cluster", c.ID, "digest", ref, "err", err)
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	defer f.Close()
	hd := w.Header()
	hd.Set("Content-Type", mediaOctetStream)
	hd.Set("Docker-Content-Digest", ref)
	hd.Set("Etag", `"`+ref+`"`)
	if r.Method != http.MethodGet {
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	pw := &countingResponse{ResponseWriter: w}
	var out http.ResponseWriter = pw
	if h.Uplink != nil && c.PacePercent > 0 && c.PacePercent < 100 {
		end := h.Uplink.begin(c.PacePercent)
		defer end()
		out = &pacedResponse{countingResponse: pw, pw: h.Uplink.writer(r.Context(), pw, c.PacePercent)}
	}
	http.ServeContent(out, r, "", time.Time{}, f)
	h.noteBytes(c.ID, pw.n)
}

// countingResponse counts the body bytes written.
type countingResponse struct {
	http.ResponseWriter
	n int64
}

func (c *countingResponse) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

// pacedResponse paces the body through the uplink.
type pacedResponse struct {
	*countingResponse
	pw io.Writer
}

func (p *pacedResponse) Write(b []byte) (int, error) { return p.pw.Write(b) }
