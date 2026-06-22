package gateway

import (
	"context"
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
// tunnel to fleetd's webhook receiver. The gateway forwards the Authorization
// header (the bearer token) untouched, so the daemon's auth stays the real
// boundary. Native gRPC is served separately on its own h2c listener (see
// grpc.go), not here.

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
