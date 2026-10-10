package server

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// control_channel.go: instances whose backend carries the control channel
// itself (backend.ControlChannel; Kubernetes pods, through their agent). The
// daemon serves nothing for them from the host control directory: it opens no
// control, SSH-agent or MCP socket there (controlRegistry.syncRunning,
// controlDirTarget), and their control files go through the channel instead
// (controlSinkFor). What reaches the daemon over the channel is served by the
// same handlers as the host sockets' connections (controlRegistry.handlerFor).

// instanceHasControlChannel reports whether inst's backend carries its control
// channel itself. A pure capability question (no I/O, nothing bound), so it is
// cheap in every reconcile loop. A package var so tests can make a channel
// instance without a backend that has one.
var instanceHasControlChannel = func(inst *fleet.Instance) bool {
	return backend.HasControlChannel(backendutil.CapabilitiesFor(inst))
}

// controlChannelFor returns inst's control channel, bound to inst, when its
// backend carries one. A package var for tests.
var controlChannelFor = func(inst *fleet.Instance) (backend.ControlChannel, bool) {
	if !instanceHasControlChannel(inst) {
		return nil, false
	}
	channel, ok := backendutil.NewForInstance(inst, false).(backend.ControlChannel)
	return channel, ok
}
