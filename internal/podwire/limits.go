package podwire

import "time"

const (
	// MaxHeader caps a preamble's JSON header.
	MaxHeader = 64 << 10
	// MaxFrame caps one frame's payload. Writers send data in DataChunk
	// frames; a reader refuses anything over MaxFrame.
	MaxFrame = 1 << 20
	// DataChunk is the largest DATA or STDERR payload a writer sends.
	DataChunk = 32 << 10
	// MaxReply caps a reply its receiver buffers whole: an RPC result, the
	// output Client.Run collects, a file Client.ReadFile returns. Streams have
	// no total cap.
	MaxReply = 16 << 20
	// SmallFile is the largest file the agent session writes or reads; bigger
	// ones go over the bulk session, so they never queue in front of PCM and
	// polls.
	SmallFile = 1 << 20
	// HeaderDeadline is how long a stream has to send its whole preamble.
	HeaderDeadline = 5 * time.Second

	// HoldBuffer is how much output a resumable exec keeps unacknowledged, per
	// stream of output, before the command's writes block; the client keeps
	// as much of its own unacknowledged input.
	HoldBuffer = 1 << 20
	// AckEvery is how many bytes a resumable stream's receiver takes in
	// between acknowledgements (PING frames carrying its offsets).
	AckEvery = 256 << 10
	// MaxHeld is how many held exec streams a pod keeps; holding one more
	// ends the oldest.
	MaxHeld = 32
	// HoldGrace is how long a held exec stream waits to be re-attached before
	// it is ended. FLEET_EXEC_HOLD_GRACE overrides it in the pod.
	HoldGrace = 12 * time.Hour

	// PingInterval is the control stream's heartbeat, each way.
	PingInterval = 15 * time.Second
	// DaemonSilence is how long fleetd waits for anything from the agent
	// before it closes the session.
	DaemonSilence = 30 * time.Second
	// AgentSilence is how long the agent waits for anything from fleetd
	// before it closes the session (and its sockets).
	AgentSilence = 45 * time.Second
	// KeepAliveInterval is yamux's own keepalive on both sessions.
	KeepAliveInterval = 10 * time.Second

	// StreamPingInterval is the heartbeat on an exec stream that crosses the
	// network on its own (a direct-mode `pod-agent exec --stdio`): it keeps
	// load-balancer idle timers and the kubelet's streaming idle timeout from
	// cutting a quiet pane. StreamSilence without a frame counts as a drop.
	StreamPingInterval = 30 * time.Second
	StreamSilence      = 90 * time.Second
)
