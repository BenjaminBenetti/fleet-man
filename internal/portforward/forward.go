package portforward

import (
	"fmt"
	"net"
	"os/exec"
)

// Forward represents a single active port forward binding.
type Forward struct {
	LocalPort    int  `json:"local_port"`
	RemotePort   int  `json:"remote_port"`
	BrowserProxy bool `json:"browser_proxy"` // true when this forward serves a browser proxy
	cmd          *exec.Cmd
	listener     net.Listener
	done         chan struct{} // closed when the in-process proxy stops
}

// Label returns a display string like "8080->80".
// Browser proxy forwards are labelled "PORT->proxy" to distinguish
// them from regular port forwards.
func (f Forward) Label() string {
	if f.BrowserProxy {
		return fmt.Sprintf("%d->proxy", f.LocalPort)
	}
	return fmt.Sprintf("%d->%d", f.LocalPort, f.RemotePort)
}

// stopForward cleanly shuts down a forward, whether it uses an
// in-process listener or an external process.
func stopForward(fwd *Forward) {
	if fwd.listener != nil {
		fwd.listener.Close()
		if fwd.done != nil {
			<-fwd.done
		}
	}
	if fwd.cmd != nil && fwd.cmd.Process != nil {
		_ = fwd.cmd.Process.Kill()
	}
}
