package portforward

import "io"

// TargetDialer opens one connection to the forward target. The proxy calls
// it once per accepted local connection. The default implementation is
// NewGRPCTarget (one FleetService.Forward stream per connection), which
// keeps forwarding correct even when the fleet server is remote.
type TargetDialer func() (io.ReadWriteCloser, error)

// closeWriter is the optional half-close half of a connection: shut the
// write side while reads continue draining. *net.TCPConn and the gRPC
// stream target both implement it.
type closeWriter interface{ CloseWrite() error }
