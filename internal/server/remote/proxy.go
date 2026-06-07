package remote

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/hashicorp/yamux"
)

// proxy.go is the data path. fleetd treats the yamux session as a net.Listener
// and runs a stdlib http.Server over it: every stream the gateway opens (one per
// inbound MCP request) is Accept()ed as a net.Conn, the HTTP request is read off
// it, and an httputil.ReverseProxy streams it to the loopback MCP server and the
// response back. Reusing http.Server + ReverseProxy means SSE flushing and
// chunked streaming are handled by the same stdlib path the local MCP server
// already uses — correct by construction, and per-stream isolation guarantees
// concurrent requests never interleave.

// serveProxy serves the yamux session until it errors (peer gone, or the caller
// closes the session to unblock it). It always returns a non-nil error — the
// listener (session) only stops by failing — which the caller uses to decide
// whether to reconnect.
func serveProxy(session *yamux.Session, mcpPort int) error {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(mcpPort))}
	rp := httputil.NewSingleHostReverseProxy(target)
	// FlushInterval -1 flushes each write immediately, which SSE (text/event-stream)
	// requires — otherwise events buffer and the stream stalls.
	rp.FlushInterval = -1
	// Set the Host to the loopback MCP target. This is REQUIRED, not cosmetic: the
	// MCP SDK enables DNS-rebinding protection by default and returns 403 when the
	// server is bound to loopback but the request's Host is not loopback — and an
	// EMPTY Host counts as non-loopback (util.IsLoopback("")==false). A tunneled
	// request would otherwise arrive with no meaningful Host and be rejected.
	// Using the loopback target satisfies the check while still rejecting a
	// genuinely foreign Host. The Authorization header is left untouched so the
	// MCP bearer token reaches the loopback server's auth — that stays the real
	// access gate.
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
	}
	// Peer resets / closed streams are normal tunnel churn, not server faults;
	// discard the proxy's per-request error spam.
	rp.ErrorLog = log.New(io.Discard, "", 0)

	srv := &http.Server{Handler: rp}
	// http.Server.Serve returns the listener's error when Accept fails; for a
	// yamux session that is the session-closed/peer-gone error we want to surface.
	return srv.Serve(session)
}
