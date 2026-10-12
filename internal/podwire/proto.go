package podwire

import "fmt"

// Protocol versions this build speaks. A version is one integer; two builds
// talk when their ranges overlap, at the highest version both speak. A change
// a peer can ignore keeps the range; a change an older peer must understand
// raises ProtoMax (both keep speaking the old version to older peers); a
// change that drops an old version raises ProtoMin. Builds whose ranges
// overlap are of the same major, and fleetd accepts an agent of any such
// build, restaging it to its own version lazily.
const (
	ProtoMin = 1
	ProtoMax = 2
)

// ProtoDesiredState is the first version whose control stream carries
// DesiredState (fleetd to the agent) and DesiredAck (back): the in-pod
// sockets and control-directory files fleetd wants. An agent that speaks only
// an older version is still accepted, and restaged lazily; until then it binds
// no sockets.
const ProtoDesiredState = 2

// Negotiate picks the version two ranges share: the highest one in both.
func Negotiate(aMin, aMax, bMin, bMax int) (int, error) {
	if aMin < 1 || aMax < aMin {
		return 0, fmt.Errorf("bad protocol range %d-%d", aMin, aMax)
	}
	if bMin < 1 || bMax < bMin {
		return 0, fmt.Errorf("bad protocol range %d-%d", bMin, bMax)
	}
	lo, hi := max(aMin, bMin), min(aMax, bMax)
	if lo > hi {
		return 0, fmt.Errorf("no common protocol version: %d-%d and %d-%d", aMin, aMax, bMin, bMax)
	}
	return hi, nil
}

// Role is which session an agent serves.
type Role string

const (
	// RoleMain is the agent session: control, polls, scripts, PCM, small
	// files and (with the features that add them) the socket relays. One
	// main agent runs per pod.
	RoleMain Role = "main"
	// RoleBulk is the bulk session, opened on demand: large files, the
	// workspace and folder tars and sync data, so they never queue in front of
	// the agent session's latency-sensitive streams.
	RoleBulk Role = "bulk"
)

// Valid reports whether r is a role.
func (r Role) Valid() bool { return r == RoleMain || r == RoleBulk }

// Hello is the agent's first message: what it is. fleetd decides from it
// whether to accept the agent; nothing in it is an identity proof (fleetd
// verifies the pod itself before it ever starts the agent).
type Hello struct {
	ProtoMin int    `json:"proto_min"`
	ProtoMax int    `json:"proto_max"`
	Role     Role   `json:"role"`
	Version  string `json:"version,omitempty"`
	// SHA is the hex sha256 of the agent's own binary.
	SHA  string `json:"sha,omitempty"`
	UID  int    `json:"uid"`
	Arch string `json:"arch,omitempty"`
	// PodUID is the pod's metadata.uid, from the downward API.
	PodUID string `json:"pod_uid,omitempty"`
	// CanSetuid is false in single-uid mode (capability tier C), where every
	// command runs as the pod's own uid.
	CanSetuid bool `json:"can_setuid"`
	PID       int  `json:"pid,omitempty"`
}

// HelloAck is fleetd's answer to Hello.
type HelloAck struct {
	Accept bool `json:"accept"`
	// Proto is the version both speak from now on.
	Proto int `json:"proto,omitempty"`
	// RestageHint tells an accepted agent that it is not the daemon's build;
	// fleetd restages it lazily.
	RestageHint bool `json:"restage_hint,omitempty"`
	// Reason says why the agent was refused.
	Reason string `json:"reason,omitempty"`
}

// ControlHeader is the control stream's preamble header: fleetd's side of the
// handshake.
type ControlHeader struct {
	ProtoMin int    `json:"proto_min"`
	ProtoMax int    `json:"proto_max"`
	Role     Role   `json:"role"`
	Version  string `json:"version,omitempty"`
}

// Control message types: the "type" of a ControlMessage.
const (
	MsgHello    = "hello"
	MsgHelloAck = "hello_ack"
	MsgLog      = "log"
	MsgClose    = "close"
	// MsgDesired carries a DesiredState (fleetd to the agent), MsgDesiredAck
	// the agent's DesiredAck once it applied one.
	MsgDesired    = "desired"
	MsgDesiredAck = "desired_ack"
	// MsgAbortStream carries a StreamAbort (fleetd to the agent): a stream
	// fleetd is done with whose agent end may still be sending.
	MsgAbortStream = "abort_stream"
)

// ControlMessage is one message on the control stream, sent as a DATA frame.
// A receiver ignores a type it does not know.
type ControlMessage struct {
	Type       string        `json:"type"`
	Hello      *Hello        `json:"hello,omitempty"`
	HelloAck   *HelloAck     `json:"hello_ack,omitempty"`
	Log        *LogRecord    `json:"log,omitempty"`
	Desired    *DesiredState `json:"desired,omitempty"`
	DesiredAck *DesiredAck   `json:"desired_ack,omitempty"`
	Abort      *StreamAbort  `json:"abort,omitempty"`
}

// StreamAbort names a TagForward stream fleetd closed for good, by its yamux
// stream id: the agent ends it (its TCP connection closed, the stream's slot
// freed) rather than relaying the pod's peer until yamux gives up on the
// half-closed stream. Its FIN alone cannot say so: that is also a
// half-close, after which the peer's answer is still wanted. An agent that
// does not know the message ignores it.
type StreamAbort struct {
	Stream uint32 `json:"stream"`
}

// LogRecord is one of the agent's own warnings or errors, for fleet.log.
type LogRecord struct {
	// Level is "error", "warn" or "info".
	Level string `json:"level"`
	Msg   string `json:"msg"`
	// Attrs are the record's key/value pairs.
	Attrs map[string]string `json:"attrs,omitempty"`
}
