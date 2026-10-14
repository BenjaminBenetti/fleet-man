package backend

import "io/fs"

// ControlChannel is an optional capability for backends that carry an
// instance's control channel themselves: the sockets a process inside reaches
// in /fleet-mounts/control (fleet.sock for `fleet launch`, fc and fo;
// ssh-agent.sock; mcp.sock) and the files the daemon keeps there (the status
// mod's). Kubernetes pods get them from the pod agent, which relays each
// connection to a socket over its session to the daemon, where it is served
// as a host socket's would be (control.ServeConn for fleet.sock).
//
// Without it the daemon serves that directory from the host: provisioning
// bind-mounts the instance's host control directory at /fleet-mounts/control,
// and the daemon listens on sockets and writes files there. For an instance
// whose backend has a control channel the daemon does none of that: it opens
// no socket in the host control directory and writes no file there, and
// provisioning mounts no host control directory into it.
//
// The handle must come from backendutil.NewForInstance, which binds the
// instance the files belong to.
type ControlChannel interface {
	// WriteControlFile makes the file name (a base name) in the instance's
	// control directory hold data, with mode. Readers inside see the old file
	// or the new one, never a partial write, and a link planted at name is
	// replaced, not followed. The file may land after the call returns (a pod
	// whose session is down gets it when the session is back).
	WriteControlFile(name string, data []byte, mode fs.FileMode) error
	// RemoveControlFile removes the file name from the instance's control
	// directory. A file that is not there is not an error.
	RemoveControlFile(name string) error
}

// HasControlChannel reports whether b carries its instances' control channel
// itself (ControlChannel). It asks a capability-only handle
// (backendutil.CapabilitiesFor) as well as a full Backend.
func HasControlChannel(b Capabilities) bool {
	_, ok := b.(ControlChannel)
	return ok
}
