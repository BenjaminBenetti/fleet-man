package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// proxy.go serves the public HTTPS endpoint. Agents connect to
// https://<host>/mcp/<publicID>[/...]; each request is reverse-proxied down the
// matching tunnel to fleetd's loopback MCP server. When the session negotiated
// FeatureGRPC, /grpc/<publicID> is also served: that one HIJACKS the connection
// and raw-splices native gRPC down the tunnel. The gateway is a transparent pipe
// — it forwards the Authorization header (the bearer token) untouched, so the
// daemon's auth stays the real boundary.

// publicHandler builds the router for the public listener.
func (s *Server) publicHandler() http.Handler {
	mux := http.NewServeMux()
	// Exact endpoint and any subpath both route to the same handler. The MCP
	// Streamable HTTP transport uses the single configured URL (POST/GET/DELETE),
	// so the exact form is the common case; the subpath form is for completeness.
	mux.HandleFunc("/mcp/{id}", s.handleMCP)
	mux.HandleFunc("/mcp/{id}/{rest...}", s.handleMCP)
	// gRPC tunnel: a hijack+splice endpoint (only live when the session negotiated
	// FeatureGRPC). The remote client opens it once and runs native gRPC over it.
	mux.HandleFunc("/grpc/{id}", s.handleGRPC)
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

// handleGRPC tunnels a native gRPC connection to fleetd. The remote fleet client
// sends a plain request to /grpc/<id>; the gateway HIJACKS the connection, opens a
// fresh (TagGRPC) yamux stream, replies "200 Connection Established", and then
// just splices raw bytes both ways. Because it never parses the payload, native
// HTTP/2 — multiple RPCs, server-streaming, and bidi — all ride transparently;
// the bearer token is checked by fleetd's gRPC interceptor (in-band metadata),
// not here.
func (s *Server) handleGRPC(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.reg.lookup(id)
	if sess == nil || !sess.grpc.Load() {
		http.Error(w, "unknown session or gRPC not available", http.StatusNotFound)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "gRPC tunnel unsupported", http.StatusInternalServerError)
		return
	}

	// Open the tunnel stream BEFORE hijacking, so a tunnel failure can still be
	// reported as a normal HTTP error.
	stream, err := sess.open(tunnel.TagGRPC)
	if err != nil {
		http.Error(w, "tunnel unavailable", http.StatusBadGateway)
		return
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = stream.Close()
		return
	}
	// From here we own clientConn; no more http.ResponseWriter use.
	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = clientConn.Close()
		_ = stream.Close()
		return
	}
	splice(clientConn, stream)
}
