package control

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Server is the host-side listener. It owns a unix socket file, accepts
// connections from in-container Clients, and invokes a handler for every
// Envelope received on any connection. One Server runs per instance the host
// wants to receive messages from; the host typically keeps a registry of them
// keyed by instance.
//
// Listen starts the accept loop in a background goroutine and returns once the
// socket is listening, so serving continues without blocking the caller.
type Server struct {
	ln         net.Listener
	socketPath string
	handler    func(Envelope)

	// mu guards conns and closed so Close can race safely against the accept
	// loop and per-connection goroutines.
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	// wg tracks the accept loop and per-connection goroutines so Close can
	// wait for them to unwind before removing the socket file.
	wg sync.WaitGroup
}

// Listen creates and serves the control socket at socketPath. It prepares the
// parent directory and a clean socket file, listens, relaxes the socket's
// permissions, and starts accepting in the background; it returns as soon as
// the socket is listening.
//
// handler is invoked for every decoded Envelope and MAY be called from
// multiple goroutines concurrently (one per open connection), so it must be
// safe for concurrent use.
//
// Setup mirrors fleet's existing bind-mount conventions:
//   - The parent directory is created 0777 because the socket is bind-mounted
//     into a container whose user may have a different UID from the host user
//     running fleet (the same rationale as the mount resolver's ensureHostDir).
//   - Any stale socket file from a prior run is removed first, since
//     net.Listen("unix", …) fails if the path already exists.
//   - After listening, the socket is chmod'd 0666 so the container user can
//     connect through the mount (the ensureHostFile rationale).
func Listen(socketPath string, handler func(Envelope)) (*Server, error) {
	// 0777 parent + remove-stale: the directory is bind-mounted cross-UID and
	// a leftover socket file from a crashed prior run would otherwise make
	// net.Listen fail with "address already in use".
	if err := os.MkdirAll(filepath.Dir(socketPath), 0777); err != nil {
		return nil, fmt.Errorf("create control dir: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale control socket: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %s: %w", socketPath, err)
	}

	// 0666 so a container user with a different UID than the host user can
	// connect to the socket through the bind mount.
	if err := os.Chmod(socketPath, 0666); err != nil {
		ln.Close()
		os.Remove(socketPath)
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}

	s := &Server{
		ln:         ln,
		socketPath: socketPath,
		handler:    handler,
		conns:      make(map[net.Conn]struct{}),
	}

	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// acceptLoop accepts connections until the listener is closed. After Close,
// Accept returns an error; the loop checks the closed flag and exits cleanly
// rather than log-spamming on the expected shutdown error.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return // expected: Close shut the listener down
			}
			// A transient accept error on a live listener is rare; keep the
			// loop alive rather than tearing the whole Server down for it.
			continue
		}

		s.mu.Lock()
		if s.closed {
			// Raced with Close after Accept returned the connection.
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go s.serveConn(conn)
	}
}

// serveConn reads successive Envelopes from one connection until EOF or a
// decode error, dispatching each to the handler. A decode error closes that
// connection (the framing is desynchronised once a line is unparseable, so the
// safe move is to drop it rather than guess where the next message starts).
func (s *Server) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer s.dropConn(conn)

	dec := json.NewDecoder(conn)
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			return // EOF or undecodable: close this connection
		}
		s.handler(env)
	}
}

// dropConn closes a connection and removes it from the tracked set. Safe to
// call once per connection; Close also closes tracked connections, so this is
// idempotent against that race via the map delete.
func (s *Server) dropConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn) // no-op if Close already removed it
	s.mu.Unlock()
	conn.Close()
}

// Close stops accepting new connections, closes every open connection, waits
// for the background goroutines to unwind, and removes the socket file. It is
// safe to call once; a second call is a no-op.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	// Close the listener first so acceptLoop exits, then close in-flight
	// connections so their serveConn goroutines hit EOF and return.
	err := s.ln.Close()
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()

	// Remove the socket file we created so the next Listen on the same path
	// starts clean even if it doesn't run the stale-file removal.
	if rmErr := os.Remove(s.socketPath); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = fmt.Errorf("remove control socket: %w", rmErr)
	}
	return err
}
