package gateway

import (
	"context"
	"net"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// control.go runs the control listener: the TLS endpoint fleetd dials to register
// a tunnel. Each accepted connection performs the internal/tunnel handshake on
// the raw conn, then becomes a yamux session the gateway opens streams on.

// serveControl accepts control connections until ctx is cancelled. sem bounds the
// number of in-flight control goroutines: a connection that can't acquire a slot
// is shed (closed) immediately, so a flood of slow/never-registering clients
// can't grow goroutines without bound or block the accept loop.
func (s *Server) serveControl(ctx context.Context, ln net.Listener, sem chan struct{}) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // listener closed on shutdown
			}
			s.log.Warn("gateway: control accept", "err", err)
			continue
		}
		select {
		case sem <- struct{}{}:
			go func(conn net.Conn) {
				defer func() { <-sem }()
				s.handleControl(ctx, conn)
			}(conn)
		default:
			s.log.Warn("gateway: control connection shed (too many in flight)", "remote", conn.RemoteAddr().String())
			_ = conn.Close()
		}
	}
}

// handleControl performs the register handshake on conn, attaches the resulting
// yamux session to the registry, and keeps the connection alive until the tunnel
// closes (fleetd gone) or the gateway shuts down.
func (s *Server) handleControl(ctx context.Context, conn net.Conn) {
	remote := conn.RemoteAddr().String()

	// Bound the pre-yamux handshake with a deadline.
	_ = conn.SetDeadline(time.Now().Add(controlHandshakeTimeout))
	var req tunnel.RegisterRequest
	if err := tunnel.ReadFrame(conn, &req); err != nil {
		s.log.Warn("gateway: read register", "remote", remote, "err", err)
		_ = conn.Close()
		return
	}

	sess, reply, isNew, err := s.reg.claim(req)
	if err != nil {
		// Best-effort tell the client why, then drop.
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: err.Error()})
		s.log.Warn("gateway: claim rejected", "remote", remote, "err", err)
		_ = conn.Close()
		return
	}
	// On any failure before bind, free a freshly-reserved slot so it doesn't count
	// against the cap (a no-op for a reclaimed, already-bound session).
	if err := tunnel.WriteFrame(conn, reply); err != nil {
		if isNew {
			s.reg.release(sess)
		}
		s.log.Warn("gateway: write register reply", "remote", remote, "err", err)
		_ = conn.Close()
		return
	}
	// Clear the handshake deadline; yamux manages its own keepalive/timeouts.
	_ = conn.SetDeadline(time.Time{})

	ym, err := tunnel.ServerSession(conn, s.yamuxLog())
	if err != nil {
		if isNew {
			s.reg.release(sess)
		}
		s.log.Warn("gateway: yamux server", "remote", remote, "err", err)
		_ = conn.Close()
		return
	}
	s.reg.bind(sess, ym)
	s.log.Info("gateway: tunnel registered", "remote", remote, "public_url", sess.publicURL)

	// Keep serving until the tunnel closes (peer gone, detected by yamux
	// keepalive) or the gateway shuts down.
	select {
	case <-ym.CloseChan():
	case <-ctx.Done():
		_ = ym.Close()
	}
	// Don't evict here: the session (and its public URL) is kept reserved for a
	// grace TTL so a reconnect with the secret recovers the same URL. The reaper
	// frees it once the TTL elapses with no reconnect. If a reconnect already
	// replaced the tunnel, the session points at the new (live) one and the
	// reaper leaves it alone.
	_ = conn.Close()
	s.log.Info("gateway: tunnel closed", "remote", remote, "public_url", sess.publicURL)
}
