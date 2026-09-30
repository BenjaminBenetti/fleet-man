package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_instance.go serves the fleet MCP server INTO the instances of automation
// agents that have the fleet MCP (Agent.FleetMCP, issue #219), so the agent can
// drive fleet from inside: spin up more instances, run agents in them. Each
// such instance gets a socket (mcpbridge.SocketName) in its control directory,
// which a process inside reaches at mcpbridge.ContainerSocketPath through the
// staged `fleet mcp-bridge`. Every connection is one MCP session in the stdio
// framing (newline-delimited JSON-RPC) over the same tools as the loopback
// HTTP server.
//
// The socket is the credential — no bearer token enters the instance — so only
// instances created for an opted-in agent get one (Instance.FleetMCP), and only
// that instance's own processes (or the daemon's user, or root) are served
// (instanceSocketSet). Devcontainer instances on a Linux host only: elsewhere
// no socket can be served into the instance.

const (
	// mcpInstanceSyncInterval is how often the sockets are reconciled against
	// the instances in state.json.
	mcpInstanceSyncInterval = 2 * time.Second
	// mcpSocketLabel names these sockets in log lines.
	mcpSocketLabel = "fleet mcp"
)

// instanceMCP owns the in-instance MCP sockets and their sessions.
type instanceMCP struct {
	svc     *service
	sockets *instanceSocketSet

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closed  bool
	serving sync.WaitGroup // session goroutines
}

func newInstanceMCP(svc *service) *instanceMCP {
	m := &instanceMCP{svc: svc, conns: make(map[net.Conn]struct{})}
	m.sockets = newInstanceSocketSet(mcpbridge.SocketName, mcpSocketLabel, m.serveFrom)
	return m
}

// run reconciles the sockets until ctx is cancelled, then closes every socket
// and session.
func (m *instanceMCP) run(ctx context.Context) {
	defer m.close()
	ticker := time.NewTicker(mcpInstanceSyncInterval)
	defer ticker.Stop()
	for {
		if st, err := state.Load(); err == nil {
			m.sync(st)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sync listens for every instance that has the fleet MCP and a mounted control
// directory, and stops listening for the rest (destroyed instances included).
func (m *instanceMCP) sync(st *state.State) {
	if st == nil {
		return
	}
	want := make(map[string]instanceTarget)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if !inst.FleetMCP {
				continue
			}
			if t, ok := controlDirTarget(fleetName, inst); ok && controlDirMounted(t.dir) {
				want[fleetName+"/"+inst.Name] = t
			}
		}
	}
	m.sockets.sync(want)
}

// ensure opens an instance's socket right away — the agent's launch calls it
// before starting the agent, so its MCP server finds the socket — or says why
// the instance cannot have one.
func (m *instanceMCP) ensure(fleetName, instanceName string) error {
	if !instanceSocketsSupported {
		return errors.New("the fleet MCP reaches into instances only on Linux hosts")
	}
	t := loadInstanceTarget(fleetName, instanceName)
	if !controlDirMounted(t.dir) {
		return errors.New("the instance does not mount its control directory (the fleet MCP needs a devcontainer instance)")
	}
	if !m.sockets.ensure(fleetName+"/"+instanceName, t) {
		return fmt.Errorf("could not listen in %s (see the log)", t.dir)
	}
	return nil
}

// controlDirMounted reports whether provisioning bind-mounted the control
// directory dir into its instance (control.MountMarkerName): only then does
// the instance see a socket served there.
func controlDirMounted(dir string) bool {
	_, err := os.Stat(control.MountMarkerPath(dir))
	return err == nil
}

// serveFrom returns the accept handler for the instance keyed key: one MCP
// session per connection, until the peer hangs up or the daemon stops.
func (m *instanceMCP) serveFrom(key string) func(net.Conn) {
	fleetName, instanceName, _ := splitInstanceKey(key)
	return func(conn net.Conn) {
		if !m.track(conn) {
			_ = conn.Close()
			return
		}
		defer m.untrack(conn)
		flog.Info("fleet mcp: session opened", "instance", key)
		srv := newMCPServer(m.svc, &mcp.ServerOptions{Instructions: instanceMCPInstructions(fleetName, instanceName)})
		err := srv.Run(m.svc.bgCtx, &mcp.IOTransport{Reader: conn, Writer: conn})
		flog.Info("fleet mcp: session closed", "instance", key, "err", err)
	}
}

// instanceMCPInstructions tells the agent where it is and how to use fleet
// from there. Claude Code shows server instructions to the model.
func instanceMCPInstructions(fleetName, instanceName string) string {
	return fmt.Sprintf(`You are running inside instance %[2]q of fleet %[1]q, started by a fleet automation. These tools drive the fleet daemon on the host that runs you: create more instances (fleet_up), run coding agents in them through sessions (fleet_session_*), and read their results — use that to split up and parallelize large work. Load the fleet-admiral skill for how the tools fit together.
Instances you create are yours to clean up: fleet_down them once their work is done. Your own instance (fleet %[1]q, instance %[2]q) is torn down automatically after you go idle — never stop, rebuild or remove it yourself.`, fleetName, instanceName)
}

// track registers a live session connection so shutdown can end it; false
// once closed.
func (m *instanceMCP) track(conn net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.conns[conn] = struct{}{}
	m.serving.Add(1)
	return true
}

func (m *instanceMCP) untrack(conn net.Conn) {
	m.mu.Lock()
	delete(m.conns, conn)
	m.mu.Unlock()
	_ = conn.Close()
	m.serving.Done()
}

// close stops every socket, ends every session, and waits for them.
func (m *instanceMCP) close() {
	m.mu.Lock()
	m.closed = true
	conns := make([]net.Conn, 0, len(m.conns))
	for conn := range m.conns {
		conns = append(conns, conn)
	}
	m.mu.Unlock()
	m.sockets.close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	m.serving.Wait()
}
