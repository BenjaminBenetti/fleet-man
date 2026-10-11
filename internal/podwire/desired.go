package podwire

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// DesiredState is what fleetd wants in a pod's control directory
// (/fleet-mounts/control): which in-pod sockets the main agent binds there,
// and the files it keeps there. fleetd sends the whole state on the control
// stream (MsgDesired) before it hands a new session out, and again on every
// change; the agent makes the directory match it and answers with a
// DesiredAck.
type DesiredState struct {
	// Gen numbers the state; the ack names it.
	Gen     uint64  `json:"gen"`
	Sockets Sockets `json:"sockets"`
	// Files are the control directory's files fleetd keeps (the status
	// mod's). A file the pod's agents wrote for an earlier state that this
	// one leaves out is removed, by whichever agent gets this one.
	Files []ControlFile `json:"files,omitempty"`
}

// Sockets says which in-pod sockets the agent binds. Each connection to one
// is relayed to fleetd on a stream of the socket's kind, which fleetd judges
// again on its own before it serves it.
type Sockets struct {
	// Control is fleet.sock (`fleet launch`, fc, fo): bound while a client
	// that can act on it (a TUI) is attached, as on docker.
	Control bool `json:"control,omitempty"`
	// SSHAgent is ssh-agent.sock: bound while the daemon can serve the pod
	// an agent. Exec-class commands get SSH_AUTH_SOCK only while it is there.
	SSHAgent bool `json:"ssh_agent,omitempty"`
	// MCP is mcp.sock: bound while the instance's fleet has Fleet MCP on
	// (fleet.rc hands agents the fleet MCP only when it is there).
	MCP bool `json:"mcp,omitempty"`
	// Host are the daemon-host sockets the devcontainer binds (a gpg agent's,
	// an X11 display's), as fleetd recorded them at create: the agent binds a
	// socket at each target and relays its connections on TagSockHost
	// streams naming the target, which fleetd dials the recorded daemon-host
	// path for.
	Host []HostSocket `json:"host,omitempty"`
	// Gateway are the daemon host's listening TCP ports, for a devcontainer
	// that declares host-gateway: the agent listens on HostGatewayAddr at
	// each and relays its connections on TagHostDial streams naming the
	// port, which fleetd dials on the daemon host.
	Gateway []uint16 `json:"gateway,omitempty"`
}

// HostSocket is a daemon-host socket bind's side in the pod: where the agent
// binds it, and the permission bits it gets (the daemon-host socket's). Its
// owner is the devcontainer's user, and so are the directories the agent
// makes for it.
type HostSocket struct {
	Target string      `json:"target"`
	Mode   fs.FileMode `json:"mode,omitempty"`
}

// HostGatewayAddr is where a pod reaches the daemon host's ports: the address
// host.docker.internal (and every name the devcontainer gives host-gateway)
// resolves to in the pod, a loopback address the agent listens on.
const HostGatewayAddr = "127.0.0.2"

// Wants reports whether s binds the in-pod socket of kind tag.
func (s Sockets) Wants(tag Tag) bool {
	switch tag {
	case TagSockControl:
		return s.Control
	case TagSockAgent:
		return s.SSHAgent
	case TagSockMCP:
		return s.MCP
	case TagSockHost:
		return len(s.Host) > 0
	case TagHostDial:
		return len(s.Gateway) > 0
	}
	return false
}

// ControlFile is one file of the control directory, written as root,
// atomically.
type ControlFile struct {
	Name string      `json:"name"`
	Mode fs.FileMode `json:"mode"`
	Data []byte      `json:"data,omitempty"`
}

// DesiredAck answers a DesiredState the agent applied. Error says what it
// could not do (a socket it could not bind, a file it could not write); the
// rest of the state applied.
type DesiredAck struct {
	Gen   uint64 `json:"gen"`
	Error string `json:"error,omitempty"`
}

// The in-pod sockets' names in the control directory: the names the
// daemon's sockets have on docker (control.SocketName, agentsock.SocketName,
// mcpbridge.SocketName, which this package, linked into the pod, does not
// import).
const (
	SockControlName = "fleet.sock"
	SockAgentName   = "ssh-agent.sock"
	SockMCPName     = "mcp.sock"
)

// SockTags are the in-pod sockets' kinds.
var SockTags = []Tag{TagSockControl, TagSockAgent, TagSockMCP}

// SockName is the control-directory name of the in-pod socket of kind tag;
// "" for a tag that is none.
func SockName(tag Tag) string {
	switch tag {
	case TagSockControl:
		return SockControlName
	case TagSockAgent:
		return SockAgentName
	case TagSockMCP:
		return SockMCPName
	}
	return ""
}

// Limits on the files a DesiredState carries: the whole state is one control
// message, under MaxFrame however its data encodes.
const (
	MaxControlFile   = 256 << 10
	MaxControlFiles  = 16
	maxControlTotal  = 512 << 10
	maxControlNameLn = 64
)

// Limits on the host relays a DesiredState carries: MaxHostSockets socket
// binds, MaxGatewayPorts mirrored ports, and a socket path a unix socket
// address holds (MaxSocketPath bytes; sun_path keeps one for its NUL).
const (
	MaxHostSockets  = 64
	MaxGatewayPorts = 512
	MaxSocketPath   = 107
)

// ValidHostSocketTarget reports whether p may be a host-socket bind's target
// in the pod: absolute, clean, not the root, short enough for a unix socket
// address, without NUL or newline.
func ValidHostSocketTarget(p string) bool {
	return p != "" && p != "/" && path.IsAbs(p) && path.Clean(p) == p && len(p) <= MaxSocketPath && !strings.ContainsAny(p, "\x00\n")
}

// ValidControlFileName reports whether name may be a control file's: a plain
// base name of letters, digits, '.', '_' and '-', not hidden (the agent's own
// files start with '.') and not an in-pod socket's.
func ValidControlFileName(name string) bool {
	if name == "" || len(name) > maxControlNameLn || name[0] == '.' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	for _, tag := range SockTags {
		if name == SockName(tag) {
			return false
		}
	}
	return true
}

// Validate checks a state's files: valid, distinct names, plain permission
// bits, and within the limits.
func (ds DesiredState) Validate() error {
	if len(ds.Files) > MaxControlFiles {
		return fmt.Errorf("%d control files, at most %d", len(ds.Files), MaxControlFiles)
	}
	seen := make(map[string]bool, len(ds.Files))
	total := 0
	for _, f := range ds.Files {
		if !ValidControlFileName(f.Name) {
			return fmt.Errorf("control file name %q is not allowed", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("control file %q is named twice", f.Name)
		}
		seen[f.Name] = true
		if f.Mode&^fs.ModePerm != 0 {
			return fmt.Errorf("control file %q: mode %v has more than permission bits", f.Name, f.Mode)
		}
		if len(f.Data) > MaxControlFile {
			return fmt.Errorf("control file %q is %d bytes, at most %d", f.Name, len(f.Data), MaxControlFile)
		}
		total += len(f.Data)
	}
	if total > maxControlTotal {
		return fmt.Errorf("control files are %d bytes in all, at most %d", total, maxControlTotal)
	}
	return ds.Sockets.validateRelays()
}

// validateRelays checks the host relays: valid, distinct targets with plain
// permission bits, distinct non-zero ports, within the limits.
func (s Sockets) validateRelays() error {
	if len(s.Host) > MaxHostSockets {
		return fmt.Errorf("%d host sockets, at most %d", len(s.Host), MaxHostSockets)
	}
	targets := make(map[string]bool, len(s.Host))
	for _, h := range s.Host {
		if !ValidHostSocketTarget(h.Target) {
			return fmt.Errorf("host socket target %q is not a clean absolute socket path", h.Target)
		}
		if targets[h.Target] {
			return fmt.Errorf("host socket target %q is named twice", h.Target)
		}
		targets[h.Target] = true
		if h.Mode&^fs.ModePerm != 0 {
			return fmt.Errorf("host socket %q: mode %v has more than permission bits", h.Target, h.Mode)
		}
	}
	if len(s.Gateway) > MaxGatewayPorts {
		return fmt.Errorf("%d gateway ports, at most %d", len(s.Gateway), MaxGatewayPorts)
	}
	ports := make(map[uint16]bool, len(s.Gateway))
	for _, p := range s.Gateway {
		if p == 0 || ports[p] {
			return fmt.Errorf("gateway port %d is zero or named twice", p)
		}
		ports[p] = true
	}
	return nil
}

// ErrNoDesiredState is SendDesired to an agent whose protocol has no
// DesiredState (below ProtoDesiredState).
var ErrNoDesiredState = errors.New("the pod agent's protocol has no desired state")
