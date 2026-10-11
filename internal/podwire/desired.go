package podwire

import (
	"errors"
	"fmt"
	"io/fs"
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
	// mod's). A file the agent wrote for an earlier state that this one
	// leaves out is removed.
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
}

// Wants reports whether s binds the in-pod socket of kind tag.
func (s Sockets) Wants(tag Tag) bool {
	switch tag {
	case TagSockControl:
		return s.Control
	case TagSockAgent:
		return s.SSHAgent
	case TagSockMCP:
		return s.MCP
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
	return nil
}

// ErrNoDesiredState is SendDesired to an agent whose protocol has no
// DesiredState (below ProtoDesiredState).
var ErrNoDesiredState = errors.New("the pod agent's protocol has no desired state")
