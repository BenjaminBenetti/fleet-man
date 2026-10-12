//go:build linux

package podagent

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// hostRelayed is what fleetd's side of a host relay test saw of one stream.
type hostRelayed struct {
	tag  podwire.Tag
	open podwire.SockOpen
	in   string
}

// hostHandlers serve the host relays as fleetd does when it serves them:
// they read the connection to its end, answer "<kind>:<target or
// port>:<what was read>" and half-close. A port in refuse is refused as one
// the daemon host no longer listens at.
func hostHandlers(seen chan<- hostRelayed, refuse ...uint16) map[podwire.Tag]podwire.Handler {
	serve := func(s *podwire.Stream) {
		var open podwire.SockOpen
		_ = podwire.DecodeHeader(s.Header, &open)
		for _, p := range refuse {
			if s.Tag == podwire.TagHostDial && open.Port == p {
				podwire.RefuseSock(s, podwire.RefusedUnreachable, "nothing listens there")
				return
			}
		}
		conn, err := podwire.AcceptSock(s)
		if err != nil {
			return
		}
		defer conn.Close()
		in, _ := io.ReadAll(conn)
		seen <- hostRelayed{tag: s.Tag, open: open, in: string(in)}
		what := open.Target
		if s.Tag == podwire.TagHostDial {
			what = strconv.Itoa(int(open.Port))
		}
		_, _ = conn.Write([]byte(s.Tag.String() + ":" + what + ":" + string(in)))
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	}
	return map[podwire.Tag]podwire.Handler{podwire.TagSockHost: serve, podwire.TagHostDial: serve}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not bound", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDesiredStateBindsTheHostSockets: the agent binds a socket at each
// host-socket bind's target, with the daemon-host socket's permission bits,
// in directories it makes 0700 where they are missing (replacing a file the
// image left there, never a directory), and relays each connection on a
// sock-host stream naming the target; one removed from under it is bound
// again, one the state leaves out is removed, a changed mode rebinds it, and
// a target in the control directory or with a directory there is refused
// (the rest still bound).
func TestDesiredStateBindsTheHostSockets(t *testing.T) {
	d := newTestDirs(t)
	root := filepath.Dir(d.control)
	seen := make(chan hostRelayed, 8)
	s := startRelayAgent(t, d, hostHandlers(seen))

	gpg := filepath.Join(root, "home", "u", ".gnupg", "S.gpg-agent")
	x0 := filepath.Join(root, "x11", "X0")
	placeholder := filepath.Join(root, "placeholder.sock")
	if err := os.WriteFile(placeholder, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want := []podwire.HostSocket{{Target: gpg, Mode: 0o700}, {Target: x0, Mode: 0o777}, {Target: placeholder}}
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Host: want}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	for path, perm := range map[string]os.FileMode{gpg: 0o700, x0: 0o777, placeholder: 0o600} {
		if m := mode(t, path); m&os.ModeSocket == 0 || m.Perm() != perm {
			t.Fatalf("%s: %v, want a %v socket", path, m, perm)
		}
	}
	for _, dir := range []string{filepath.Dir(gpg), filepath.Dir(filepath.Dir(gpg)), filepath.Dir(x0)} {
		if m := mode(t, dir); !m.IsDir() || m.Perm() != 0o700 {
			t.Fatalf("made %s %v, want 0700", dir, m)
		}
	}
	if out := talk(t, gpg, "sign"); out != "sock-host:"+gpg+":sign" {
		t.Fatalf("answered %q", out)
	}
	if got := <-seen; got.tag != podwire.TagSockHost || got.open.Target != gpg || got.open.Peer == nil || got.open.Peer.UID != os.Getuid() || got.in != "sign" {
		t.Fatalf("fleetd saw %+v", got)
	}

	// Removed from under the agent: bound again.
	_ = os.Remove(gpg)
	waitSocket(t, gpg)
	if out := talk(t, gpg, "again"); out != "sock-host:"+gpg+":again" {
		t.Fatalf("answered %q", out)
	}
	<-seen

	// A directory at a target, and a target in the control directory, are
	// refused; the others stay bound.
	dirTarget := filepath.Join(root, "a-dir")
	if err := os.Mkdir(dirTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	inControl := filepath.Join(d.control, "evil.sock")
	before, _ := os.Lstat(x0)
	ack := apply(t, s, podwire.DesiredState{Gen: 2, Sockets: podwire.Sockets{Host: []podwire.HostSocket{
		{Target: gpg, Mode: 0o700}, {Target: x0, Mode: 0o777}, {Target: dirTarget}, {Target: inControl},
	}}})
	if !strings.Contains(ack.Error, dirTarget+" is a directory") || !strings.Contains(ack.Error, inControl) {
		t.Fatalf("ack %+v", ack)
	}
	if _, err := os.Lstat(inControl); !os.IsNotExist(err) {
		t.Fatalf("a socket was bound in the control directory: %v", err)
	}
	if _, err := os.Lstat(placeholder); !os.IsNotExist(err) {
		t.Fatalf("a socket the state left out is still there: %v", err)
	}
	if after, err := os.Lstat(x0); err != nil || !os.SameFile(before, after) {
		t.Fatalf("a socket the state kept was bound again: %v", err)
	}
	if r, ok := s.logs.find("the control directory is not as fleetd wants it", 5*time.Second); !ok || r.Level != slog.LevelError {
		t.Fatalf("the refused targets were not logged as errors: %+v", r)
	}

	// A new mode is a new socket.
	apply(t, s, podwire.DesiredState{Gen: 3, Sockets: podwire.Sockets{Host: []podwire.HostSocket{{Target: x0, Mode: 0o770}}}})
	if m := mode(t, x0); m&os.ModeSocket == 0 || m.Perm() != 0o770 {
		t.Fatalf("x0 %v after its mode changed", m)
	}
	if _, err := os.Lstat(gpg); !os.IsNotExist(err) {
		t.Fatalf("gpg is still bound: %v", err)
	}

	// The session ends: its sockets go with it.
	_ = s.c.Close()
	select {
	case <-s.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not end")
	}
	if _, err := os.Lstat(x0); !os.IsNotExist(err) {
		t.Fatalf("x0 outlived the session: %v", err)
	}
}

// freeGatewayPorts are n distinct ports nothing listens at on
// HostGatewayAddr (nor on every address: a wildcard listener holds the port
// there too). They are held together until all are picked: one port let go
// before the next is asked for can be handed out again.
func freeGatewayPorts(t *testing.T, n int) []uint16 {
	t.Helper()
	ports := make([]uint16, 0, n)
	for range n {
		ln, err := net.Listen("tcp4", podwire.HostGatewayAddr+":0")
		if err != nil {
			t.Skipf("cannot listen on %s: %v", podwire.HostGatewayAddr, err)
		}
		defer ln.Close()
		ports = append(ports, uint16(ln.Addr().(*net.TCPAddr).Port))
	}
	return ports
}

func gatewayAddr(p uint16) string {
	return net.JoinHostPort(podwire.HostGatewayAddr, strconv.Itoa(int(p)))
}

// talkTCP connects to addr, sends msg, half-closes and returns the answer.
func talkTCP(t *testing.T, addr, msg string) (string, error) {
	t.Helper()
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		return "", err
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	out, err := io.ReadAll(conn)
	return string(out), err
}

// TestDesiredStateMirrorsTheGatewayPorts: the agent listens on
// HostGatewayAddr at each gateway port and relays each connection on a
// host-dial stream naming the port, half-close included; a port the pod
// itself listens at on every interface is left to the pod (no error); a
// connection fleetd refuses is reset, not ended as if served; a port the
// state leaves out stops being mirrored.
func TestDesiredStateMirrorsTheGatewayPorts(t *testing.T) {
	d := newTestDirs(t)
	seen := make(chan hostRelayed, 8)
	// The pod's own listener first: the ports picked next cannot be its.
	pods, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pods.Close()
	podsPort := uint16(pods.Addr().(*net.TCPAddr).Port)
	free := freeGatewayPorts(t, 2)
	served, refused := free[0], free[1]
	go func() {
		for {
			c, err := pods.Accept()
			if err != nil {
				return
			}
			_, _ = io.ReadAll(c)
			_, _ = c.Write([]byte("the pod's own"))
			c.Close()
		}
	}()

	s := startRelayAgent(t, d, hostHandlers(seen, refused))
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Gateway: []uint16{served, refused, podsPort}}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	out, err := talkTCP(t, gatewayAddr(served), "GET /")
	if err != nil || out != "host-dial:"+strconv.Itoa(int(served))+":GET /" {
		t.Fatalf("answered %q, %v", out, err)
	}
	if got := <-seen; got.tag != podwire.TagHostDial || got.open.Port != served || got.in != "GET /" {
		t.Fatalf("fleetd saw %+v", got)
	}
	if out, err := talkTCP(t, gatewayAddr(podsPort), "x"); err != nil || out != "the pod's own" {
		t.Fatalf("a port the pod listens at: %q, %v", out, err)
	}
	// A client that sends nothing and waits: a reset, not an empty answer.
	// On a loaded machine the reset can beat the client's own connect.
	conn, err := net.Dial("tcp4", gatewayAddr(refused))
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err = io.ReadAll(conn)
		conn.Close()
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("a refused connection: %v, want a reset", err)
	}
	select {
	case got := <-seen:
		t.Fatalf("fleetd served %+v", got)
	default:
	}

	// The pod's listener goes: the next check mirrors its port.
	pods.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := talkTCP(t, gatewayAddr(podsPort), "late")
		if out == "host-dial:"+strconv.Itoa(int(podsPort))+":late" {
			<-seen
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a port the pod let go was not mirrored: %q", out)
		}
		time.Sleep(controlCheck)
	}

	apply(t, s, podwire.DesiredState{Gen: 2, Sockets: podwire.Sockets{Gateway: []uint16{refused}}})
	if _, err := talkTCP(t, gatewayAddr(served), "x"); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a port the state left out: %v", err)
	}
}

// TestHostSocketsAreTheUsersAsRoot: a root agent gives each host socket to
// the devcontainer's user: bound as root, then given to the user, in a
// directory only root can change; bound as the user in the user's own; the
// directories it makes on the way the user's and 0700, made by root only
// inside root's directories. A link the user planted in their own directory
// is followed as the user, so it never leads the agent anywhere the user
// could not bind.
func TestHostSocketsAreTheUsersAsRoot(t *testing.T) {
	requireRoot(t)
	d := newTestDirs(t)
	root := filepath.Dir(d.control)
	o := d.options()
	o.ControlCheck = controlCheck
	o.Host = rootHost(t, filepath.Join(root, "fsroot"))
	home := filepath.Join(root, "fsroot", "home")
	seen := make(chan hostRelayed, 8)
	s, err := tryStartAgent(t, o, podwire.ClientOptions{Handlers: hostHandlers(seen)})
	if err != nil {
		t.Fatal(err)
	}

	run := filepath.Join(root, "run")
	mkdir(t, run, 0o755, 0, 0)
	etc := filepath.Join(root, "etc")
	mkdir(t, etc, 0o755, 0, 0)
	if err := os.Symlink(etc, filepath.Join(home, "evil")); err != nil {
		t.Fatal(err)
	}
	_ = os.Lchown(filepath.Join(home, "evil"), rootTestUID, rootTestUID)

	inRun := filepath.Join(run, "itest-host-gpg-agent.sock")
	nested := filepath.Join(run, "user", "4242", "gnupg", "S.gpg-agent")
	inHome := filepath.Join(home, ".gnupg", "S.gpg-agent")
	planted := filepath.Join(home, "evil", "S.planted")
	ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Host: []podwire.HostSocket{
		{Target: inRun, Mode: 0o700}, {Target: nested, Mode: 0o700}, {Target: inHome, Mode: 0o600}, {Target: planted},
	}}})
	if !strings.Contains(ack.Error, planted) || strings.Count(ack.Error, "bind the host socket") != 1 {
		t.Fatalf("ack %+v, want only the planted target refused", ack)
	}
	for _, path := range []string{inRun, nested, inHome} {
		if uid, gid := owner(t, path); uid != rootTestUID || gid != rootTestUID {
			t.Fatalf("%s is %d:%d, want the user's", path, uid, gid)
		}
		if out := talk(t, path, "x"); out != "sock-host:"+path+":x" {
			t.Fatalf("%s answered %q", path, out)
		}
		<-seen
	}
	for _, dir := range []string{filepath.Join(run, "user"), filepath.Dir(filepath.Dir(nested)), filepath.Dir(nested), filepath.Dir(inHome)} {
		if uid, _ := owner(t, dir); uid != rootTestUID || mode(t, dir).Perm() != 0o700 {
			t.Fatalf("made %s %v owned by %d, want the user's, 0700", dir, mode(t, dir), uid)
		}
	}
	if uid, _ := owner(t, run); uid != 0 {
		t.Fatalf("%s changed hands", run)
	}
	if _, err := os.Lstat(filepath.Join(etc, "S.planted")); !os.IsNotExist(err) {
		t.Fatalf("the agent bound through the user's link into root's directory: %v", err)
	}

	// Unbound as whoever bound them.
	apply(t, s, podwire.DesiredState{Gen: 2})
	for _, path := range []string{inRun, nested, inHome} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", path, err)
		}
	}
}
