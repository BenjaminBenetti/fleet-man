package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// proxy.go serves the public HTTP(S) endpoint. Agents connect to
// <public-url>/mcp/<publicID>[/...]; each request is reverse-proxied down the
// matching tunnel to fleetd's loopback MCP server. Remote systems POST automation
// events to <public-url>/webhook/<publicID>/<name>, reverse-proxied down the same
// tunnel to fleetd's webhook receiver. Cluster nodes pull a daemon's images at
// <host>/v2/<rid>/<repository>/{manifests,blobs}/<digest>, reverse-proxied down a
// TagRegistry stream (over the session's registry connection when one is
// attached) to fleetd's pull-only image registry. The gateway forwards the
// Authorization header (the bearer token, or a node's Basic pull credential)
// untouched, so the daemon's auth stays the real boundary. Native gRPC is served
// separately on its own h2c listener (see grpc.go), not here.

// publicHandler builds the router for the public listener.
func (s *Server) publicHandler() http.Handler {
	mux := http.NewServeMux()
	// Exact endpoint and any subpath both route to the same handler. The MCP
	// Streamable HTTP transport uses the single configured URL (POST/GET/DELETE),
	// so the exact form is the common case; the subpath form is for completeness.
	mux.HandleFunc("/mcp/{id}", s.handleMCP)
	mux.HandleFunc("/mcp/{id}/{rest...}", s.handleMCP)
	// Webhook delivery (issue #193): the trailing segment is the user-named
	// webhook the trigger matches on. The subpath form lets a sender append extra
	// path the remote system may require; fleetd routes on the first segment.
	mux.HandleFunc("/webhook/{id}/{name}", s.handleWebhook)
	mux.HandleFunc("/webhook/{id}/{name}/{rest...}", s.handleWebhook)
	// Image pulls (gateway delivery): the OCI distribution API root, answered by
	// the gateway itself, and every per-daemon registry path under its rid.
	mux.HandleFunc("/v2/{$}", s.handleRegistryBase)
	mux.HandleFunc("/v2/{rid}/{rest...}", s.handleRegistry)
	// Native gRPC is NOT served here — it has its own h2c listener (see grpc.go),
	// because mixing h2c and HTTP/1.1 on one port is brittle and an L7 gRPC proxy
	// needs a clean HTTP/2 endpoint.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// handleMCP reverse-proxies one request to fleetd over its tunnel.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.reg.lookup(id)
	if sess == nil {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	// Strip the /mcp/<id> prefix: fleetd's MCP server is mounted at root, so the
	// public /mcp/<id> maps to "/" (plus any subpath).
	target := "/" + r.PathValue("rest")

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "tunnel" // ignored: the transport dials the yamux stream
			req.URL.Path = target
			// Blank the Host so fleetd presents the loopback target to its MCP
			// server (matching the loopback proxy on the fleetd side).
			req.Host = ""
		},
		Transport: &http.Transport{
			// One fresh yamux stream per request; no pooling of tunnel streams.
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return sess.open(tunnel.TagMCP)
			},
		},
		// Flush each write immediately so SSE (text/event-stream) responses stream
		// through instead of buffering.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			// A tunnel that just dropped / is mid-reconnect: report a clean 502.
			http.Error(w, "tunnel unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// handleWebhook reverse-proxies one inbound automation event to fleetd's webhook
// receiver over a TagWebhook tunnel stream. The session must have negotiated the
// webhook feature (the user enabled "Enable Webhook"); otherwise the route is
// dead and we 404 — identical treatment to an unknown id, so a probe can't tell a
// webhook-disabled daemon from a non-existent one.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.reg.lookup(id)
	if sess == nil || !sess.webhook.Load() {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	// fleetd's webhook receiver routes on the path, so forward the segments AFTER
	// the session id: "/<name>" plus any extra path the sender appended. fleetd
	// matches triggers on the first segment (the webhook name).
	target := "/" + r.PathValue("name")
	if rest := r.PathValue("rest"); rest != "" {
		target += "/" + rest
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "tunnel" // ignored: the transport dials the yamux stream
			req.URL.Path = target
			req.Host = "" // fleetd's receiver doesn't care about Host
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return sess.open(tunnel.TagWebhook)
			},
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "tunnel unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// registryRealm is the Basic realm the gateway and fleetd's image registry
// challenge with.
const registryRealm = "fleet"

// writeRegistryError answers a registry request with an OCI distribution error
// body ({"errors":[{"code","message"}]}), which docker, CRI-O and containerd
// clients parse, and the distribution API version header.
func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Docker-Distribution-API-Version", "registry/2.0")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

// handleRegistryBase answers the bare API root /v2/ itself, with a Basic
// challenge. containerd never requests it, but docker (cri-dockerd nodes
// included) and CRI-O probe it first and take the auth scheme from its
// challenge, then send the pull secret's Basic credential with every request
// under the host. The root names no daemon, so the gateway cannot check a
// credential here; fleetd checks it on every manifest and blob request. A
// gateway that does not offer the registry feature 404s, like any unknown
// path.
func (s *Server) handleRegistryBase(w http.ResponseWriter, r *http.Request) {
	if s.reg.registryHost == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+registryRealm+`"`)
	writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

// handleRegistry reverse-proxies one image pull request to fleetd's registry
// over a TagRegistry stream. The session must have negotiated the registry
// feature (the user enabled "Enable Image Registry"); otherwise the route is
// dead and we 404, identical treatment to an unknown rid, so a probe cannot
// tell a registry-disabled daemon from a non-existent one. The registry is
// pull-only, so only GET and HEAD reach the daemon.
//
// fleetd serves its images at the API root (/v2/<repository>/...), so the rid
// is stripped: /v2/<rid>/devcontainer/manifests/sha256:… reaches fleetd as
// /v2/devcontainer/manifests/sha256:…. Range, Authorization and the response's
// Content-Type, Content-Length and Docker-Content-Digest pass through untouched.
//
// At most maxRegistryStreams requests per session are in flight at once; a
// request past the cap waits for a slot (or for its client to give up). While
// the daemon is reconnecting the request gets 502, and once the session is
// reaped (sessionTTL after the daemon dropped) 404.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	sess := s.reg.lookupRegistry(r.PathValue("rid"))
	if sess == nil || !sess.registry.Load() {
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown or expired registry")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "the registry is pull-only")
		return
	}

	select {
	case sess.registrySlots <- struct{}{}:
		defer func() { <-sess.registrySlots }()
	case <-r.Context().Done():
		return // the puller gave up while waiting for a slot
	}

	target := "/v2/" + r.PathValue("rest")
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "tunnel" // ignored: the transport dials the yamux stream
			req.URL.Path = target
			req.URL.RawPath = ""
			req.Host = "" // fleetd's registry serves one host; it doesn't care
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return sess.openRegistry()
			},
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeRegistryError(w, http.StatusBadGateway, "UNAVAILABLE", "tunnel unavailable")
		},
	}
	rp.ServeHTTP(w, r)
}
