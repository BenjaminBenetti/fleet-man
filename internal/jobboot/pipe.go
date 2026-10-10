package jobboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultDialTimeout is how long RunPipe retries the job socket: pod-job opens
// it a moment after the wait loop sees .ready.
const DefaultDialTimeout = 30 * time.Second

// RunPipe is `fleet pod-job feed`: a thin pipe between an exec's stdio and the
// job socket, so fleetd can run Feed over a plain kube exec. It dials socket
// (retrying for up to dialTimeout, 0 is DefaultDialTimeout), copies stdin to
// the socket and the socket to stdout, half-closes the socket when stdin ends,
// and returns once the job closes its end. All protocol lives in Feed and the
// Server; a pipe that dies loses nothing the job cannot get again.
func RunPipe(ctx context.Context, socket string, stdin io.Reader, stdout io.Writer, dialTimeout time.Duration) error {
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	conn, err := dialRetry(ctx, socket, dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		_, _ = io.Copy(conn, stdin)
		_ = conn.CloseWrite()
	}()
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdout, conn)
		copied <- err
	}()
	select {
	case err := <-copied:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func dialRetry(ctx context.Context, socket string, timeout time.Duration) (*net.UnixConn, error) {
	deadline := time.Now().Add(timeout)
	var d net.Dialer
	for {
		c, err := d.DialContext(ctx, "unix", socket)
		if err == nil {
			return c.(*net.UnixConn), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial the job socket %s: %w", socket, err)
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		}
	}
}
