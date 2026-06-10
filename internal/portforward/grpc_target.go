package portforward

import (
	"context"
	"io"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// grpc_target.go is the CLIENT half of the port-forward data plane (issue
// #81): each accepted local connection opens one FleetService.Forward bidi
// stream, sends the ForwardOpen header, then speaks raw bytes. The SERVER
// owns the bridge into the container (internal/server/forward.go), so the
// forward holds even when the server is remote — unlike the old path, which
// spawned a server-computed argv on the client host.

// NewGRPCTarget returns a TargetDialer that tunnels one connection per call
// over svc's Forward stream to remotePort inside fleet/instance.
func NewGRPCTarget(svc fleetgrpc.FleetServiceClient, fleetName, instance string, remotePort int) TargetDialer {
	return func() (io.ReadWriteCloser, error) {
		// The stream lives as long as the forwarded connection: no deadline,
		// torn down by Close (cancel) or by the server ending the stream.
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := svc.Forward(ctx)
		if err != nil {
			cancel()
			return nil, err
		}
		open := &fleetgrpc.ForwardChunk{Msg: &fleetgrpc.ForwardChunk_Open{Open: &fleetgrpc.ForwardOpen{
			Fleet:      fleetName,
			Instance:   instance,
			RemotePort: int32(remotePort),
		}}}
		if err := stream.Send(open); err != nil {
			cancel()
			return nil, err
		}
		return &streamConn{stream: stream, cancel: cancel}, nil
	}
}

// streamConn adapts a Forward stream to io.ReadWriteCloser (+ half-close).
// The proxy's two pumps put Read and Write on separate goroutines, which is
// exactly gRPC's stream contract: one sender, one receiver.
type streamConn struct {
	stream fleetgrpc.FleetService_ForwardClient
	cancel context.CancelFunc
	rbuf   []byte // unconsumed remainder of the last received chunk
}

func (c *streamConn) Read(p []byte) (int, error) {
	for len(c.rbuf) == 0 {
		chunk, err := c.stream.Recv()
		if err != nil {
			return 0, err // io.EOF on a clean stream end
		}
		c.rbuf = chunk.GetData()
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *streamConn) Write(p []byte) (int, error) {
	if err := c.stream.Send(&fleetgrpc.ForwardChunk{Msg: &fleetgrpc.ForwardChunk_Data{Data: p}}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite half-closes the upload (CloseSend): the server half-closes its
// in-container bridge while the download keeps draining.
func (c *streamConn) CloseWrite() error { return c.stream.CloseSend() }

// Close tears the whole stream down by cancelling the RPC.
func (c *streamConn) Close() error {
	c.cancel()
	return nil
}
