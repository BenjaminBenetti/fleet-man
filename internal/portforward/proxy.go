package portforward

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// startProxy opens a TCP listener on localPort and proxies each accepted
// connection to a target opened via dial. Accepting stops when the listener
// is closed; live connections are tracked on the Forward so stopForward can
// kill them too.
func startProxy(localPort, remotePort int, dial TargetDialer) (*Forward, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", localPort))
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	fwd := &Forward{
		LocalPort:  localPort,
		RemotePort: remotePort,
		listener:   ln,
		done:       done,
		conns:      newConnSet(),
	}

	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			fwd.conns.add(conn)
			go func() {
				defer fwd.conns.remove(conn)
				proxy(conn, dial)
			}()
		}
	}()

	return fwd, nil
}

// proxy pipes data between a local connection and a freshly dialed target.
func proxy(local net.Conn, dial TargetDialer) {
	remote, err := dial()
	if err != nil {
		flog.Warn("port-forward: dial target", "err", err)
		local.Close()
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(remote, local)
		closeWrite(remote)
	}()

	go func() {
		defer wg.Done()
		io.Copy(local, remote)
		closeWrite(local)
	}()

	wg.Wait()
	local.Close()
	remote.Close()
}

// closeWrite half-closes c's write side when supported, so the peer sees EOF
// while its remaining replies drain.
func closeWrite(c io.Closer) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
