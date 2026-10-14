package sshserver

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// forward.go serves the client's forwards into the instance: direct-tcpip
// (ssh -L, -W, -D; VS Code reaching its server and auto-forwarding ports)
// and direct-streamlocal (ssh -L to a unix socket). Each is dialled from
// inside the instance, so loopback-only services are reachable, and EOF in
// either direction is passed on as a half-close.

// dialTimeout bounds one forward's dial.
const dialTimeout = 10 * time.Second

func (c *connection) handleDirectTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil || p.Port == 0 || p.Port > 65535 {
		_ = nc.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
	c.forward(nc, "tcp", addr)
}

func (c *connection) handleDirectStreamLocal(nc ssh.NewChannel) {
	var p struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil || p.SocketPath == "" {
		_ = nc.Reject(ssh.ConnectionFailed, "malformed direct-streamlocal request")
		return
	}
	c.forward(nc, "unix", p.SocketPath)
}

// forward dials network/address in the instance and pipes it to the channel.
func (c *connection) forward(nc ssh.NewChannel, network, address string) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(c.ctx, network, address)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	c.vscode.noteForward(network, address)
	pipe(ch, reqs, conn)
}

// pipe copies both ways until both are done. A direction's EOF half-closes
// the other side; an error, or the client closing the channel, ends both.
func pipe(ch ssh.Channel, reqs <-chan *ssh.Request, conn net.Conn) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = ch.Close()
		})
	}
	go func() {
		ssh.DiscardRequests(reqs) // returns once the channel is closed
		closeAll()
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(conn, ch); err != nil {
			closeAll()
			return
		}
		if !closeWrite(conn) {
			closeAll()
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(ch, conn); err != nil && !errors.Is(err, net.ErrClosed) {
			closeAll()
			return
		}
		_ = ch.CloseWrite()
	}()
	wg.Wait()
	closeAll()
}

// closeWrite half-closes conn; false when it cannot.
func closeWrite(conn net.Conn) bool {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() == nil
	}
	return false
}
