package remote

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
)

// proxy.go is the data path. fleetd treats the yamux session as a source of
// streams and serves each one.
//
// Two modes, chosen per connection by what the gateway negotiated:
//   - serveProxy (legacy / MCP-only gateway): every stream is an HTTP request,
//     served by one http.Server over the whole session and reverse-proxied to the
//     loopback MCP server.
//   - serveTunnel (FeatureGRPC negotiated): every stream begins with a tag byte;
//     fleetd reads it and dispatches — TagMCP streams go to the MCP http.Server,
//     TagGRPC streams are spliced (as raw net.Conns) to the daemon's tunnel-facing
//     gRPC server. One stream per request/connection means MCP requests, SSE, and
//     native gRPC (incl. bidi) never interleave.

// mcpReverseProxy builds the reverse proxy to the loopback MCP server.
func mcpReverseProxy(mcpPort int) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(mcpPort))}
	rp := httputil.NewSingleHostReverseProxy(target)
	// FlushInterval -1 flushes each write immediately, which SSE (text/event-stream)
	// requires — otherwise events buffer and the stream stalls.
	rp.FlushInterval = -1
	// Set the Host to the loopback MCP target. This is REQUIRED, not cosmetic: the
	// MCP SDK enables DNS-rebinding protection by default and returns 403 when the
	// server is bound to loopback but the request's Host is not loopback (an EMPTY
	// Host counts as non-loopback). Using the loopback target satisfies the check
	// while still rejecting a genuinely foreign Host. Authorization is left
	// untouched so the MCP bearer token reaches the loopback server's auth gate.
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
	}
	// Peer resets / closed streams are normal tunnel churn, not server faults.
	rp.ErrorLog = log.New(io.Discard, "", 0)
	return rp
}

// serveProxy serves an MCP-only session: every stream is an HTTP request. It
// returns when the session errors (peer gone, or the caller closes it).
func serveProxy(session *yamux.Session, mcpPort int) error {
	srv := &http.Server{Handler: mcpReverseProxy(mcpPort)}
	return srv.Serve(session)
}

// serveReject parks on a session that may serve NOTHING (remote fleet only, but
// the gateway did not negotiate gRPC): every stream the gateway pushes is closed
// unanswered. Keeping the session itself open preserves the sticky registration
// until the user re-enables a traffic kind. Returns when the session errors.
func serveReject(session *yamux.Session) error {
	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		_ = stream.Close()
	}
}

// serveTunnel serves a session whose streams are TAG-prefixed (FeatureGRPC
// negotiated): it reads each stream's tag and routes it. MCP streams feed a local
// http.Server — unless remote MCP is disabled (mcpOn false: a remote-fleet-only
// tunnel), in which case they are rejected; gRPC streams feed grpcLis (the
// daemon's tunnel-facing gRPC server, which is shared across reconnects and NOT
// closed here). Returns when the session errors.
func serveTunnel(session *yamux.Session, mcpPort int, grpcLis *ChanListener, mcpOn bool) error {
	var mcpLis *ChanListener
	if mcpOn {
		mcpLis = NewChanListener()
		mcpSrv := &http.Server{Handler: mcpReverseProxy(mcpPort)}
		// Closing the server closes mcpLis (so its Accept unblocks) and drops the
		// per-connection MCP requests; grpcLis is NOT closed — it outlives this
		// connection so reconnects reuse the same gRPC server.
		defer mcpSrv.Close()
		go func() { _ = mcpSrv.Serve(mcpLis) }()
	}

	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go dispatchStream(stream, mcpLis, grpcLis)
	}
}

// dispatchStream reads the leading tag byte and routes the (tag-stripped) stream
// to the MCP or gRPC listener. A nil listener means that traffic kind is
// disabled, so its streams are rejected. An unreadable tag or unknown value
// closes the stream without disturbing the others.
func dispatchStream(stream net.Conn, mcpLis, grpcLis *ChanListener) {
	tag, err := tunnel.ReadTag(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	switch {
	case tag == tunnel.TagMCP && mcpLis != nil:
		mcpLis.Push(stream)
	case tag == tunnel.TagGRPC && grpcLis != nil:
		grpcLis.Push(stream)
	default:
		_ = stream.Close()
	}
}
