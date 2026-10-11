package backendtest

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// forwardTimeout bounds one exchange with a server in the instance.
const forwardTimeout = 30 * time.Second

// halfCloser is a connection that can end its write side and keep reading.
type halfCloser interface{ CloseWrite() error }

// lateReply is how long after the client's EOF the late server answers:
// past the second a cluster's port-forward keeps a half-closed connection.
const lateReply = 2 * time.Second

// caseForward: every way the backend offers to reach a port inside the
// instance works: PortDialer (and its half-close path, HalfCloseDialer), a
// direct dial to ResolveHostname's address (which is only offered when it is
// reachable), and ForwardStdioCommand. Each carries data both ways, and where
// it can half-close, the server sees EOF while its reply still comes back
// (the Forward data plane relies on it). A half-close path also keeps a reply
// that comes seconds after the EOF; whether the others do is recorded.
func caseForward(c *C) {
	i := c.Shared()
	if !c.NeedTool(i, "socat") {
		return
	}
	echoPort := 40000 + rand.IntN(5000)
	countPort := echoPort + 5000
	latePort := echoPort + 10000
	pids := c.Script(i, fmt.Sprintf(
		`socat TCP-LISTEN:%d,reuseaddr,fork EXEC:cat >/dev/null 2>&1 & echo $!; socat TCP-LISTEN:%d,reuseaddr,fork 'EXEC:wc -c' >/dev/null 2>&1 & echo $!; socat TCP-LISTEN:%d,reuseaddr,fork 'SYSTEM:cat >/dev/null; sleep %d; echo late' >/dev/null 2>&1 & echo $!`,
		echoPort, countPort, latePort, int(lateReply/time.Second)))
	c.Cleanup(func() { _, _ = i.B.RunScript(i.ID(), "kill "+strings.Join(strings.Fields(pids), " ")+" 2>/dev/null") })
	for _, port := range []int{echoPort, countPort, latePort} {
		c.Eventually(fmt.Sprintf("the in-instance server on port %d", port), 30*time.Second, func() (bool, string) {
			r := c.Exec(i, ShArgv(`socat -u /dev/null TCP:127.0.0.1:"$1"`, strconv.Itoa(port)), nil)
			return r.Err == nil, r.String()
		})
	}

	surfaces := 0
	if dialer, ok := i.B.(backend.PortDialer); ok {
		surfaces++
		dial := func(port int) (io.ReadWriteCloser, error) { return dialer.DialPort(c.Ctx, i.ID(), port) }
		checkForward(c, "PortDialer", dial, echoPort, countPort)
		recordLateReply(c, "PortDialer", dial, latePort)
	}
	if hc, ok := i.B.(backend.HalfCloseDialer); ok {
		surfaces++
		dial := func(port int) (io.ReadWriteCloser, error) { return hc.DialPortHalfClose(c.Ctx, i.ID(), port) }
		checkForward(c, "HalfCloseDialer", dial, echoPort, countPort)
		if got, err := lateExchange(dial, latePort); err != nil || got != "late" {
			c.Errorf("HalfCloseDialer: a reply %v after the half-close should arrive; got %q (err %v)", lateReply, got, err)
		}
	}
	if host, ok := i.B.ResolveHostname(i.ID()); ok {
		surfaces++
		dial := func(port int) (io.ReadWriteCloser, error) {
			return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
		}
		checkForward(c, "ResolveHostname "+host, dial, echoPort, countPort)
		recordLateReply(c, "ResolveHostname", dial, latePort)
	}
	if _, ok := i.B.ForwardStdioCommand(i.ID(), echoPort); ok {
		surfaces++
		dial := func(port int) (io.ReadWriteCloser, error) {
			cmd, _ := i.B.ForwardStdioCommand(i.ID(), port)
			return startStdio(cmd)
		}
		checkForward(c, "ForwardStdioCommand", dial, echoPort, countPort)
		recordLateReply(c, "ForwardStdioCommand", dial, latePort)
	}
	if surfaces == 0 {
		c.Errorf("no way to reach a port in the instance: no PortDialer, no reachable ResolveHostname, no ForwardStdioCommand")
	}
}

// checkForward runs an echo exchange and, where the connection half-closes, a
// half-close exchange through one surface.
func checkForward(c *C, surface string, dial func(port int) (io.ReadWriteCloser, error), echoPort, countPort int) {
	c.Helper()
	conn, err := dial(echoPort)
	if err != nil {
		c.Errorf("%s: dial the echo server: %v", surface, err)
		return
	}
	payload := randomBytes(256<<10, 4)
	got, err := exchange(conn, payload, false, len(payload))
	_ = conn.Close()
	if err != nil || !bytes.Equal(got, payload) {
		c.Errorf("%s: 256 KiB echoed back as %d bytes (err %v)", surface, len(got), err)
	}

	conn, err = dial(countPort)
	if err != nil {
		c.Errorf("%s: dial the counting server: %v", surface, err)
		return
	}
	defer conn.Close()
	if _, ok := conn.(halfCloser); !ok {
		c.Logf("%s: its connections cannot half-close; skipping the half-close exchange", surface)
		return
	}
	got, err = exchange(conn, payload, true, -1)
	if err != nil || strings.TrimSpace(string(got)) != strconv.Itoa(len(payload)) {
		c.Errorf("%s: after a half-close the server should count %d bytes and reply; got %q (err %v)", surface, len(payload), got, err)
	}
}

// lateExchange sends a line, half-closes and reads the late server's answer.
func lateExchange(dial func(port int) (io.ReadWriteCloser, error), port int) (string, error) {
	conn, err := dial(port)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, ok := conn.(halfCloser); !ok {
		return "", fmt.Errorf("the connection cannot half-close")
	}
	got, err := exchange(conn, []byte("ask\n"), true, -1)
	return strings.TrimSpace(string(got)), err
}

// recordLateReply logs whether a surface keeps a reply that comes seconds
// after the half-close (a cluster's port-forward closes about a second after
// it; a stdio bridge's socat half a second): parity, not a requirement.
func recordLateReply(c *C, surface string, dial func(port int) (io.ReadWriteCloser, error), port int) {
	start := time.Now()
	got, err := lateExchange(dial, port)
	c.Logf("%s: a reply %v after the half-close: %q (err %v, connection ended after %v)", surface, lateReply, got, err, time.Since(start).Round(100*time.Millisecond))
}

// exchange writes payload to conn (then half-closes it when halfClose) while
// reading: want bytes, or with want < 0 everything until EOF.
func exchange(conn io.ReadWriteCloser, payload []byte, halfClose bool, want int) ([]byte, error) {
	if d, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now().Add(forwardTimeout))
	} else {
		timer := time.AfterFunc(forwardTimeout, func() { _ = conn.Close() })
		defer timer.Stop()
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		if err == nil && halfClose {
			err = conn.(halfCloser).CloseWrite()
		}
		wrote <- err
	}()
	var got []byte
	var err error
	if want >= 0 {
		got = make([]byte, want)
		_, err = io.ReadFull(conn, got)
	} else {
		got, err = io.ReadAll(conn)
	}
	if werr := <-wrote; err == nil {
		err = werr
	}
	return got, err
}

// stdioConn is a ForwardStdioCommand process as a connection: its stdin and
// stdout are the two directions, and closing stdin half-closes.
type stdioConn struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
}

func startStdio(cmd *exec.Cmd) (*stdioConn, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stdioConn{cmd: cmd, in: in, out: out}, nil
}

func (s *stdioConn) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *stdioConn) Write(p []byte) (int, error) { return s.in.Write(p) }
func (s *stdioConn) CloseWrite() error           { return s.in.Close() }

func (s *stdioConn) Close() error {
	_ = s.in.Close()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	return nil
}
