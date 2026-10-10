package podwire

import "fmt"

// Tag is a stream's kind: the first byte of its preamble.
type Tag byte

// Streams fleetd opens on the agent session (0x10-0x15), streams the agent
// opens (0x16, 0x20-0x23, 0x32) and the shared-state streams (0x30-0x32). The
// relay and sync tags are named here, beside the ones this package serves, so
// the numbering has one home; their handlers come with those features.
const (
	// TagControl is the control stream: the handshake, the heartbeat, the
	// agent's LOG messages and fleetd's Close. fleetd opens exactly one, first.
	TagControl Tag = 0x10
	// TagRPC is one buffered request and its reply (Client.Call).
	TagRPC Tag = 0x11
	// TagExec runs a command, or re-attaches a held one (Client.Exec).
	TagExec Tag = 0x12
	// TagFileWrite writes one file atomically as the exec user.
	TagFileWrite Tag = 0x13
	// TagFileRead reads one file as the exec user.
	TagFileRead Tag = 0x14
	// TagForward is a TCP connection to a pod port, the fallback when the
	// cluster denies pods/portforward.
	TagForward Tag = 0x15
	// TagHostDial is a pod connection to a daemon-host port (host-gateway).
	TagHostDial Tag = 0x16
	// TagSockControl, TagSockAgent, TagSockMCP and TagSockHost relay one
	// connection to an in-pod socket: fleet.sock, ssh-agent.sock, mcp.sock and
	// a recorded host-socket bind.
	TagSockControl Tag = 0x20
	TagSockAgent   Tag = 0x21
	TagSockMCP     Tag = 0x22
	TagSockHost    Tag = 0x23
	// TagSyncCtl and TagShareHint carry shared-state sync control and change
	// hints on the agent session; TagSyncBulk carries sync data on the bulk
	// session.
	TagSyncCtl   Tag = 0x30
	TagSyncBulk  Tag = 0x31
	TagShareHint Tag = 0x32
)

var tagNames = map[Tag]string{
	TagControl:     "control",
	TagRPC:         "rpc",
	TagExec:        "exec",
	TagFileWrite:   "file-write",
	TagFileRead:    "file-read",
	TagForward:     "forward",
	TagHostDial:    "host-dial",
	TagSockControl: "sock-control",
	TagSockAgent:   "sock-agent",
	TagSockMCP:     "sock-mcp",
	TagSockHost:    "sock-host",
	TagSyncCtl:     "sync-ctl",
	TagSyncBulk:    "sync-bulk",
	TagShareHint:   "share-hint",
}

func (t Tag) String() string {
	if name, ok := tagNames[t]; ok {
		return name
	}
	return fmt.Sprintf("tag-0x%02x", byte(t))
}

// Known reports whether t is a tag this protocol defines.
func (t Tag) Known() bool {
	_, ok := tagNames[t]
	return ok
}

// streamCaps is how many streams of each kind the accepting side holds open at
// once; a stream over its kind's cap is refused. The socket relay caps are the
// plan's (control 32, agent 64, mcp 16, host 32); the others bound what a
// misbehaving peer can pin.
var streamCaps = map[Tag]int{
	TagControl:     1,
	TagRPC:         32,
	TagExec:        128,
	TagFileWrite:   16,
	TagFileRead:    16,
	TagForward:     64,
	TagHostDial:    32,
	TagSockControl: 32,
	TagSockAgent:   64,
	TagSockMCP:     16,
	TagSockHost:    32,
	TagSyncCtl:     1,
	TagSyncBulk:    8,
	TagShareHint:   1,
}

// StreamCap is how many streams of kind t one side accepts at once.
func StreamCap(t Tag) int {
	return streamCaps[t]
}
