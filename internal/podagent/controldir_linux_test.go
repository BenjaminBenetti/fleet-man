//go:build linux

package podagent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// relayed is what fleetd's side of a relay test saw of one connection.
type relayed struct {
	tag  podwire.Tag
	peer *podwire.Peer
	in   string
}

// echoHandlers serve every in-pod socket's relay stream as fleetd would
// when its gate is open: they read the connection to its end, then answer
// "<kind>:<what was read>" and half-close. gated kinds are refused instead.
func echoHandlers(seen chan<- relayed, gated ...podwire.Tag) map[podwire.Tag]podwire.Handler {
	h := map[podwire.Tag]podwire.Handler{}
	for _, tag := range podwire.SockTags {
		h[tag] = func(s *podwire.Stream) {
			for _, g := range gated {
				if g == tag {
					podwire.RefuseSock(s, podwire.RefusedGate, "closed for the test")
					return
				}
			}
			var open podwire.SockOpen
			_ = podwire.DecodeHeader(s.Header, &open)
			conn, err := podwire.AcceptSock(s)
			if err != nil {
				return
			}
			defer conn.Close()
			in, _ := io.ReadAll(conn)
			seen <- relayed{tag: tag, peer: open.Peer, in: string(in)}
			_, _ = conn.Write([]byte(tag.String() + ":" + string(in)))
			_ = conn.(interface{ CloseWrite() error }).CloseWrite()
		}
	}
	return h
}

// controlCheck is the tests' socket check interval.
const controlCheck = 50 * time.Millisecond

// startRelayAgent is a main agent whose fleetd side serves the relays with
// handlers.
func startRelayAgent(t *testing.T, d testDirs, handlers map[podwire.Tag]podwire.Handler) *session {
	t.Helper()
	o := d.options()
	o.ControlCheck = controlCheck
	s, err := tryStartAgent(t, o, podwire.ClientOptions{Handlers: handlers})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// apply sends ds and waits for the agent's ack.
func apply(t *testing.T, s *session, ds podwire.DesiredState) podwire.DesiredAck {
	t.Helper()
	if err := s.c.SendDesired(ds); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack, err := s.c.WaitDesired(ctx, ds.Gen)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

// talk connects to the in-pod socket at path, sends msg, half-closes, and
// returns the answer.
func talk(t *testing.T, path, msg string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = conn.(*net.UnixConn).CloseWrite()
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(out)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func waitNoFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still there", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDesiredStateBindsAndRelaysTheInPodSockets: the agent binds the sockets
// the state asks for, 0666 in a 0755 control directory, and relays each
// connection to fleetd on a stream of its kind with the peer's credentials,
// half-close included; a later state unbinds what it leaves out.
func TestDesiredStateBindsAndRelaysTheInPodSockets(t *testing.T) {
	d := newTestDirs(t)
	seen := make(chan relayed, 8)
	s := startRelayAgent(t, d, echoHandlers(seen))
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Control: true, SSHAgent: true, MCP: true}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	if m := mode(t, d.control); !m.IsDir() || m.Perm() != 0o755 {
		t.Fatalf("control dir %v", m)
	}
	for _, tag := range podwire.SockTags {
		path := filepath.Join(d.control, podwire.SockName(tag))
		if m := mode(t, path); m&os.ModeSocket == 0 || m.Perm() != 0o666 {
			t.Fatalf("%s: %v", path, m)
		}
		if out := talk(t, path, "hello "+tag.String()); out != tag.String()+":hello "+tag.String() {
			t.Fatalf("%s answered %q", path, out)
		}
		got := <-seen
		if got.tag != tag || got.peer == nil || got.peer.UID != os.Getuid() || got.peer.PID != os.Getpid() || got.in != "hello "+tag.String() {
			t.Fatalf("fleetd saw %+v", got)
		}
	}

	control := filepath.Join(d.control, podwire.SockControlName)
	before, _ := os.Lstat(control)
	if ack := apply(t, s, podwire.DesiredState{Gen: 2, Sockets: podwire.Sockets{Control: true}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	for _, name := range []string{podwire.SockAgentName, podwire.SockMCPName} {
		if _, err := os.Lstat(filepath.Join(d.control, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still bound: %v", name, err)
		}
	}
	if after, err := os.Lstat(control); err != nil || !os.SameFile(before, after) {
		t.Fatalf("a socket the state kept was bound again: %v", err)
	}
	if out := talk(t, control, "still"); out != "sock-control:still" {
		t.Fatalf("answered %q", out)
	}
}

// TestDesiredStateKeepsTheControlFiles: the state's files are written
// (atomically, with their mode), rewritten when the pod lost one, and
// removed once a state leaves them out; the agent's own files are never
// touched.
func TestDesiredStateKeepsTheControlFiles(t *testing.T) {
	d := newTestDirs(t)
	s := startRelayAgent(t, d, nil)
	status := podwire.ControlFile{Name: "claude-status.json", Mode: 0o644, Data: []byte(`{"live":false}`)}
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Files: []podwire.ControlFile{status}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	path := filepath.Join(d.control, status.Name)
	if data, _ := os.ReadFile(path); string(data) != `{"live":false}` || mode(t, path).Perm() != 0o644 {
		t.Fatalf("file %q mode %v", data, mode(t, path))
	}
	// The pod lost it: the next state with the same content writes it again.
	_ = os.Remove(path)
	apply(t, s, podwire.DesiredState{Gen: 2, Files: []podwire.ControlFile{status}})
	if data, _ := os.ReadFile(path); string(data) != `{"live":false}` {
		t.Fatalf("not repaired: %q", data)
	}
	status.Data = []byte(`{"live":true}`)
	apply(t, s, podwire.DesiredState{Gen: 3, Files: []podwire.ControlFile{status}})
	if data, _ := os.ReadFile(path); string(data) != `{"live":true}` {
		t.Fatalf("not updated: %q", data)
	}
	other := filepath.Join(d.control, "not-fleetds.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	apply(t, s, podwire.DesiredState{Gen: 4})
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("a file the state left out is still there: %v", err)
	}
	if _, err := os.Lstat(other); err != nil {
		t.Fatalf("a file the agent never wrote was removed: %v", err)
	}
	entries, _ := os.ReadDir(d.control)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("a temp file was left: %s", e.Name())
		}
	}
}

// TestALaterAgentRemovesTheFilesAnEarlierOneWrote: the next agent of the pod
// (a reconnect, a takeover) removes the files an earlier one wrote for
// fleetd that its first state leaves out (the status mod turned off while
// the pod had no session), and keeps writing the ones it keeps; files no
// agent wrote stay.
func TestALaterAgentRemovesTheFilesAnEarlierOneWrote(t *testing.T) {
	d := newTestDirs(t)
	first := startRelayAgent(t, d, nil)
	status := podwire.ControlFile{Name: "claude-status.json", Mode: 0o644, Data: []byte(`{"live":false}`)}
	other := podwire.ControlFile{Name: "other.json", Mode: 0o600, Data: []byte(`{}`)}
	apply(t, first, podwire.DesiredState{Gen: 1, Files: []podwire.ControlFile{status, other}})
	_ = first.c.Close()
	select {
	case <-first.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the first agent did not end")
	}
	notFleetds := filepath.Join(d.control, "not-fleetds.txt")
	if err := os.WriteFile(notFleetds, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	next := startRelayAgent(t, d, nil)
	other.Data = []byte(`{"n":2}`)
	if ack := apply(t, next, podwire.DesiredState{Gen: 1, Files: []podwire.ControlFile{other}}); ack.Error != "" {
		t.Fatalf("ack %+v", ack)
	}
	if _, err := os.Lstat(filepath.Join(d.control, status.Name)); !os.IsNotExist(err) {
		t.Fatalf("a file an earlier agent wrote, which the state leaves out, is still there: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(d.control, other.Name)); string(data) != `{"n":2}` {
		t.Fatalf("a kept file %q", data)
	}
	if _, err := os.Lstat(notFleetds); err != nil {
		t.Fatalf("a file no agent wrote was removed: %v", err)
	}
	if m := mode(t, filepath.Join(d.control, filesManifestName)); !m.IsRegular() || m.Perm() != 0o600 {
		t.Fatalf("manifest %v", m)
	}
	apply(t, next, podwire.DesiredState{Gen: 2})
	for _, name := range []string{other.Name, filesManifestName} {
		if _, err := os.Lstat(filepath.Join(d.control, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still there with no files wanted: %v", name, err)
		}
	}
}

// TestDesiredStateBindsAReplacedSocketAgain: a socket whose file was removed
// is bound again; one whose name something else holds fails, once, as an
// error fleetd writes to fleet.log; ending the session removes the sockets.
func TestDesiredStateBindsAReplacedSocketAgain(t *testing.T) {
	d := newTestDirs(t)
	seen := make(chan relayed, 8)
	s := startRelayAgent(t, d, echoHandlers(seen))
	apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Control: true, SSHAgent: true}})
	control := filepath.Join(d.control, podwire.SockControlName)
	agent := filepath.Join(d.control, podwire.SockAgentName)

	_ = os.Remove(control)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, err := os.Lstat(control); err == nil && m.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a removed socket was not bound again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out := talk(t, control, "again"); out != "sock-control:again" {
		t.Fatalf("answered %q", out)
	}
	<-seen

	// Something the agent cannot remove takes the agent socket's name.
	_ = os.Remove(agent)
	if err := os.MkdirAll(filepath.Join(agent, "full"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, ok := s.logs.find("the control directory is not as fleetd wants it", 5*time.Second)
	if !ok {
		t.Fatal("the failed bind was not logged")
	}
	if rec.Level != slog.LevelError || !strings.Contains(recordAttrs(rec), podwire.SockAgentName) {
		t.Fatalf("logged %v %s", rec.Level, recordAttrs(rec))
	}
	time.Sleep(10 * controlCheck)
	if n := s.logs.count("the control directory is not as fleetd wants it"); n != 1 {
		t.Fatalf("the failure was logged %d times, want once", n)
	}
	if ack := apply(t, s, podwire.DesiredState{Gen: 2, Sockets: podwire.Sockets{Control: true, SSHAgent: true}}); !strings.Contains(ack.Error, podwire.SockAgentName) {
		t.Fatalf("ack %+v", ack)
	}

	_ = s.c.Close()
	waitNoFile(t, control)
	if _, err := os.Stat(filepath.Join(agent, "full")); err != nil {
		t.Fatalf("the session's end removed what was not its socket: %v", err)
	}
}

// TestDesiredStateGateRefusalEndsTheConnection: a connection fleetd refuses
// (its gate is closed) is ended at once, and is no warning of the agent's.
func TestDesiredStateGateRefusalEndsTheConnection(t *testing.T) {
	d := newTestDirs(t)
	seen := make(chan relayed, 1)
	s := startRelayAgent(t, d, echoHandlers(seen, podwire.TagSockMCP))
	apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{MCP: true}})
	conn, err := net.Dial("unix", filepath.Join(d.control, podwire.SockMCPName))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if out, err := io.ReadAll(conn); len(out) != 0 || os.IsTimeout(err) {
		t.Fatalf("a refused connection answered %q (%v)", out, err)
	}
	if _, ok := s.logs.find("could not relay", 200*time.Millisecond); ok {
		t.Fatal("a gate refusal was logged as a relay failure")
	}
}

// TestBulkAgentKeepsNoControlDirectory: only the main agent binds sockets.
func TestBulkAgentKeepsNoControlDirectory(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.Role = podwire.RoleBulk
	s := startAgent(t, o)
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Control: true}}); !strings.Contains(ack.Error, "keeps no control directory") {
		t.Fatalf("ack %+v", ack)
	}
	if _, err := os.Lstat(filepath.Join(d.control, podwire.SockControlName)); !os.IsNotExist(err) {
		t.Fatalf("the bulk agent bound a socket: %v", err)
	}
}

func recordAttrs(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.Key + "=" + a.Value.String() + " ")
		return true
	})
	return b.String()
}

// count is how many records' messages contain msg.
func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if strings.Contains(r.Message, msg) {
			n++
		}
	}
	return n
}

// TestTakeoverHandsTheSocketsOver: the agent that releases to a new one
// unbinds its sockets before it lets the lock go, so the new agent's bind is
// never undone by the old one's exit.
func TestTakeoverHandsTheSocketsOver(t *testing.T) {
	d := newTestDirs(t)
	old := startHelperAgent(t, d)
	if err := old.c.SendDesired(podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Control: true}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ack, err := old.c.WaitDesired(ctx, 1); err != nil || ack.Error != "" {
		t.Fatalf("old agent: %+v %v", ack, err)
	}
	control := filepath.Join(d.control, podwire.SockControlName)
	if m := mode(t, control); m&os.ModeSocket == 0 {
		t.Fatalf("old agent's socket %v", m)
	}

	seen := make(chan relayed, 4)
	o := takeoverOptions(d, 2*time.Second, 2*time.Second)
	o.ControlCheck = controlCheck
	s, err := tryStartAgent(t, o, podwire.ClientOptions{Handlers: echoHandlers(seen)})
	if err != nil {
		t.Fatal(err)
	}
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, Sockets: podwire.Sockets{Control: true}}); ack.Error != "" {
		t.Fatalf("new agent: %+v", ack)
	}
	select {
	case <-old.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the old agent did not exit")
	}
	time.Sleep(10 * controlCheck)
	if out := talk(t, control, "after takeover"); out != "sock-control:after takeover" {
		t.Fatalf("answered %q", out)
	}
	<-seen
}

// TestProbeDepsAreTheInPodSockets: the exec class's probe cache follows the
// sockets this agent binds that a shell's startup may test (fleet.rc's mcp.sock,
// a user's rc the agent socket), but for fleet.sock, which every TUI attach
// and detach binds and unbinds (execas tests fleet.rc's own against the deps).
func TestProbeDepsAreTheInPodSockets(t *testing.T) {
	for _, tag := range podwire.SockTags {
		dep := filepath.Join(DefaultControlDir, podwire.SockName(tag))
		if want := tag != podwire.TagSockControl; slices.Contains(execas.DefaultProbeDeps, dep) != want {
			t.Errorf("execas.DefaultProbeDeps %q: %s in it is %v, want %v", execas.DefaultProbeDeps, dep, !want, want)
		}
	}
}
