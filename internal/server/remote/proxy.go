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
//   - serveTunnel (a tagging feature negotiated: FeatureGRPC, FeatureWebhook
//     and/or FeatureRegistry, see tunnel.Tagged): every stream begins with a tag
//     byte; fleetd reads it and dispatches — TagMCP streams go to the MCP
//     http.Server, TagGRPC streams are spliced (as raw net.Conns) to the daemon's
//     tunnel-facing gRPC server, TagWebhook streams go to the webhook receiver's
//     http.Server, TagRegistry streams to the image server. One stream per
//     request/connection means MCP requests, SSE, native gRPC (incl. bidi),
//     webhooks and image pulls never interleave.

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

// tunnelListeners are the listeners tagged tunnel streams are pushed to, one per
// non-MCP traffic kind. They are shared across reconnects and never closed by
// the serve loops; a nil listener means that traffic kind was not negotiated,
// so its streams are rejected.
type tunnelListeners struct {
	grpc     *ChanListener
	webhook  *ChanListener
	registry *ChanListener
	// registryHit, when set, is called for every registry stream (the main
	// tunnel's, to open the registry connection).
	registryHit func()
}

// serveTunnel serves a session whose streams are TAG-prefixed (a tagging feature
// negotiated, see tunnel.Tagged): it reads each stream's tag and routes it. MCP
// streams feed a local http.Server — unless remote MCP is disabled (mcpOn false:
// a remote-fleet/webhook/registry-only tunnel), in which case they are rejected;
// gRPC, webhook and registry streams feed their listener in lis. Returns when
// the session errors.
func serveTunnel(session *yamux.Session, mcpPort int, lis tunnelListeners, mcpOn bool) error {
	var mcpLis *ChanListener
	if mcpOn {
		mcpLis = NewChanListener()
		mcpSrv := &http.Server{Handler: mcpReverseProxy(mcpPort)}
		// Closing the server closes mcpLis (so its Accept unblocks) and drops the
		// per-connection MCP requests; grpcLis/webhookLis are NOT closed — they
		// outlive this connection so reconnects reuse the same servers.
		defer mcpSrv.Close()
		go func() { _ = mcpSrv.Serve(mcpLis) }()
	}

	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go dispatchStream(stream, mcpLis, lis)
	}
}

// dispatchStream reads the leading tag byte and routes the (tag-stripped) stream
// to the MCP, gRPC, webhook or registry listener. A nil listener means that
// traffic kind is disabled, so its streams are rejected. An unreadable tag or
// unknown value closes the stream without disturbing the others. A registry
// attach connection (tunnel.AttachRegistry) carries only TagRegistry streams, so
// it can serve with dispatchStream(stream, nil, tunnelListeners{registry: …}).
func dispatchStream(stream net.Conn, mcpLis *ChanListener, lis tunnelListeners) {
	tag, err := tunnel.ReadTag(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	switch {
	case tag == tunnel.TagMCP && mcpLis != nil:
		mcpLis.Push(stream)
	case tag == tunnel.TagGRPC && lis.grpc != nil:
		lis.grpc.Push(stream)
	case tag == tunnel.TagWebhook && lis.webhook != nil:
		lis.webhook.Push(stream)
	case tag == tunnel.TagRegistry && lis.registry != nil:
		if lis.registryHit != nil {
			lis.registryHit()
		}
		lis.registry.Push(stream)
	default:
		_ = stream.Close()
	}
}
