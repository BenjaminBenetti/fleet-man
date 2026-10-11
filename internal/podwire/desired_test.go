package podwire

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"strings"
	"testing"
	"time"
)

func desiredHello() Hello {
	h := goodHello()
	h.ProtoMax = ProtoDesiredState
	return h
}

// readDesired is the fake agent reading the next DesiredState fleetd sent.
func (fa *fakeAgent) readDesired(t *testing.T) DesiredState {
	t.Helper()
	for {
		f, err := fa.ctlR.Next()
		if err != nil {
			t.Fatalf("control stream: %v", err)
		}
		var m ControlMessage
		if f.Type == FrameData && DecodeFrame(f, &m) == nil && m.Type == MsgDesired && m.Desired != nil {
			return *m.Desired
		}
	}
}

// TestDesiredStateRoundTrip: fleetd sends the whole state on the control
// stream and waits for the agent's ack of that generation (or a later one);
// an older ack does not satisfy the wait.
func TestDesiredStateRoundTrip(t *testing.T) {
	c, fa, err := dialFake(t, desiredHello(), ClientOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.SupportsDesired() || c.Proto() != ProtoDesiredState {
		t.Fatalf("proto %d", c.Proto())
	}
	ds := DesiredState{Gen: 7, Sockets: Sockets{Control: true, MCP: true},
		Files: []ControlFile{{Name: "claude-status.json", Mode: 0o644, Data: []byte(`{"live":true}`)}}}
	if err := c.SendDesired(ds); err != nil {
		t.Fatal(err)
	}
	got := fa.readDesired(t)
	if got.Gen != 7 || !got.Sockets.Control || got.Sockets.SSHAgent || !got.Sockets.MCP || len(got.Files) != 1 ||
		string(got.Files[0].Data) != `{"live":true}` || got.Files[0].Mode != 0o644 {
		t.Fatalf("the agent got %+v", got)
	}

	waited := make(chan error, 1)
	var ack DesiredAck
	go func() {
		var err error
		ack, err = c.WaitDesired(context.Background(), 7)
		waited <- err
	}()
	_ = fa.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgDesiredAck, DesiredAck: &DesiredAck{Gen: 6}})
	select {
	case err := <-waited:
		t.Fatalf("an older ack ended the wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = fa.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgDesiredAck, DesiredAck: &DesiredAck{Gen: 7, Error: "bind failed"}})
	if err := <-waited; err != nil || ack.Gen != 7 || ack.Error != "bind failed" {
		t.Fatalf("ack %+v err %v", ack, err)
	}
	// A later ack answers a wait for an earlier state at once.
	_ = fa.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgDesiredAck, DesiredAck: &DesiredAck{Gen: 9}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if ack, err := c.WaitDesired(ctx, 8); err != nil || ack.Gen == 9 {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
}

// TestDesiredStateNeedsTheProtocol: an agent of protocol 1 gets no state, and
// a wait ends with the session or its context.
func TestDesiredStateNeedsTheProtocol(t *testing.T) {
	c, _, err := dialFake(t, goodHello(), ClientOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.SupportsDesired() || !errors.Is(c.SendDesired(DesiredState{Gen: 1}), ErrNoDesiredState) {
		t.Fatal("a protocol 1 agent was sent a desired state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.WaitDesired(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	_ = c.Close()
	if _, err := c.WaitDesired(context.Background(), 1); err == nil {
		t.Fatal("a wait on a closed session")
	}
}

func TestDesiredStateValidate(t *testing.T) {
	file := func(name string, n int) ControlFile {
		return ControlFile{Name: name, Mode: 0o644, Data: make([]byte, n)}
	}
	ok := DesiredState{Files: []ControlFile{file("claude-status.json", 100), file("other_1.txt", 0)}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	many := DesiredState{}
	for i := 0; i <= MaxControlFiles; i++ {
		many.Files = append(many.Files, file("f"+strings.Repeat("x", i), 1))
	}
	for name, ds := range map[string]DesiredState{
		"hidden":       {Files: []ControlFile{file(".pod-agent.pid", 1)}},
		"path":         {Files: []ControlFile{file("a/b", 1)}},
		"dotdot":       {Files: []ControlFile{file("..", 1)}},
		"socket name":  {Files: []ControlFile{file("ssh-agent.sock", 1)}},
		"twice":        {Files: []ControlFile{file("a", 1), file("a", 2)}},
		"setuid":       {Files: []ControlFile{{Name: "a", Mode: 0o4755}}},
		"directory":    {Files: []ControlFile{{Name: "a", Mode: fs.ModeDir | 0o755}}},
		"too large":    {Files: []ControlFile{file("a", MaxControlFile+1)}},
		"too many":     many,
		"too much":     {Files: []ControlFile{file("a", MaxControlFile), file("b", MaxControlFile), file("c", 1)}},
		"long name":    {Files: []ControlFile{file(strings.Repeat("n", 65), 1)}},
		"control char": {Files: []ControlFile{file("a\nb", 1)}},
	} {
		if err := ds.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, tag := range SockTags {
		if SockName(tag) == "" || ValidControlFileName(SockName(tag)) {
			t.Errorf("%s: socket name %q", tag, SockName(tag))
		}
		if !(Sockets{Control: true, SSHAgent: true, MCP: true}).Wants(tag) || (Sockets{}).Wants(tag) {
			t.Errorf("%s: Wants", tag)
		}
	}
	if SockName(TagExec) != "" || (Sockets{Control: true, SSHAgent: true, MCP: true}).Wants(TagSockHost) {
		t.Error("a socket for a kind that is none")
	}
}

// TestSockRelayHalfClose: a relay stream the agent opens is answered by
// fleetd's handler; each side's end of input reaches the other as EOF while
// the other direction still flows, and Close ends a reader the other side
// never ends.
func TestSockRelayHalfClose(t *testing.T) {
	served := make(chan []byte, 1)
	handlers := map[Tag]Handler{TagSockControl: func(s *Stream) {
		var open SockOpen
		if DecodeHeader(s.Header, &open) != nil || open.Peer == nil || open.Peer.UID != 1000 {
			RefuseSock(s, RefusedGate, "no peer")
			return
		}
		conn, err := AcceptSock(s)
		if err != nil {
			return
		}
		defer conn.Close()
		in, _ := io.ReadAll(conn) // to the agent's half-close
		served <- in
		_, _ = conn.Write([]byte("reply after your EOF"))
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	}}
	_, fa, err := dialFake(t, desiredHello(), ClientOptions{Handlers: handlers}, nil)
	if err != nil {
		t.Fatal(err)
	}
	up, err := DialSock(fa.sess, TagSockControl, SockOpen{Peer: &Peer{PID: 7, UID: 1000, GID: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := up.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := <-served; string(got) != "request" {
		t.Fatalf("fleetd read %q", got)
	}
	reply, err := io.ReadAll(up)
	if err != nil || string(reply) != "reply after your EOF" {
		t.Fatalf("reply %q %v", reply, err)
	}
	_ = up.Close()

	// A refusal names its reason; a kind with no handler is the Mux's own.
	if _, err := DialSock(fa.sess, TagSockControl, SockOpen{}); !IsSockRefused(err, RefusedGate) || !strings.Contains(err.Error(), "no peer") {
		t.Fatalf("refused: %v", err)
	}
	if _, err := DialSock(fa.sess, TagSockMCP, SockOpen{}); !IsSockRefused(err, RefusedUnsupported) {
		t.Fatalf("unserved kind: %v", err)
	}
}

// TestSockConnCloseEndsAStuckRead: Close of fleetd's side ends its own read
// even though the agent never sends its FIN.
func TestSockConnCloseEndsAStuckRead(t *testing.T) {
	conns := make(chan net.Conn, 1)
	release := make(chan struct{})
	handlers := map[Tag]Handler{TagSockAgent: func(s *Stream) {
		conn, err := AcceptSock(s)
		if err != nil {
			return
		}
		conns <- conn
		<-release
	}}
	_, fa, err := dialFake(t, desiredHello(), ClientOptions{Handlers: handlers}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer close(release)
	up, err := DialSock(fa.sess, TagSockAgent, SockOpen{})
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	conn := <-conns
	read := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		read <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a read after Close returned data")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close left the read blocked")
	}
}
