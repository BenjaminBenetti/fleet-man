package podwire

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/hashicorp/yamux"
)

// Exec is what the k8s handle hands the daemon's Exec RPC, mic and output
// as a backend.ExecStream.
var _ backend.ExecStream = (*Exec)(nil)

// fakeAgent is the agent's side of a session, scripted by each test.
type fakeAgent struct {
	sess   *yamux.Session
	header ControlHeader
	ctlW   *FrameWriter
	ctlR   *FrameReader
	ack    HelloAck
}

// startFakeAgent runs the agent side of the handshake with hello over conn and
// then serves handlers.
func startFakeAgent(t *testing.T, conn net.Conn, hello Hello, handlers map[Tag]Handler) <-chan *fakeAgent {
	t.Helper()
	out := make(chan *fakeAgent, 1)
	go func() {
		defer close(out)
		sess, err := yamux.Server(conn, SessionConfig(hello.Role))
		if err != nil {
			return
		}
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		tag, raw, err := ReadPreamble(st)
		if err != nil || tag != TagControl {
			return
		}
		fa := &fakeAgent{sess: sess, ctlW: NewFrameWriter(st), ctlR: NewFrameReader(st)}
		_ = DecodeHeader(raw, &fa.header)
		_ = fa.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgHello, Hello: &hello})
		for {
			f, err := fa.ctlR.Next()
			if err != nil {
				return
			}
			var m ControlMessage
			if DecodeFrame(f, &m) == nil && m.Type == MsgHelloAck {
				fa.ack = *m.HelloAck
				break
			}
		}
		go func() { _ = (&Mux{Handlers: handlers}).Serve(sess) }()
		out <- fa
	}()
	return out
}

func dialFake(t *testing.T, hello Hello, opts ClientOptions, handlers map[Tag]Handler) (*Client, *fakeAgent, error) {
	t.Helper()
	a, b := net.Pipe()
	agentCh := startFakeAgent(t, a, hello, handlers)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, b, opts)
	fa := <-agentCh
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, fa, err
}

func goodHello() Hello {
	return Hello{ProtoMin: 1, ProtoMax: 1, Role: RoleMain, Version: "v1.2.3", UID: 0, Arch: "amd64", PodUID: "pod-1", CanSetuid: true, PID: 42}
}

func TestDialHandshake(t *testing.T) {
	c, fa, err := dialFake(t, goodHello(), ClientOptions{Version: "v1.2.3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fa.header.Role != RoleMain || fa.header.ProtoMin != ProtoMin || fa.header.ProtoMax != ProtoMax || fa.header.Version != "v1.2.3" {
		t.Fatalf("control header %+v", fa.header)
	}
	if !fa.ack.Accept || fa.ack.Proto != 1 || fa.ack.RestageHint {
		t.Fatalf("ack %+v", fa.ack)
	}
	if c.Hello().PodUID != "pod-1" || c.Proto() != 1 || c.RestageHint() {
		t.Fatalf("client hello %+v proto %d", c.Hello(), c.Proto())
	}
}

func TestDialAcceptsAnotherBuildWithRestageHint(t *testing.T) {
	hello := goodHello()
	hello.Version, hello.ProtoMax = "v1.0.0", 3
	c, fa, err := dialFake(t, hello, ClientOptions{Version: "v1.2.3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fa.ack.Accept || !fa.ack.RestageHint || fa.ack.Proto != ProtoMax || !c.RestageHint() {
		t.Fatalf("ack %+v", fa.ack)
	}
}

func TestDialRefuses(t *testing.T) {
	newer := goodHello()
	newer.ProtoMin, newer.ProtoMax = ProtoMax+1, ProtoMax+2
	bulk := goodHello()
	bulk.Role = RoleBulk
	cases := map[string]struct {
		hello Hello
		opts  ClientOptions
		want  string
	}{
		"no common version": {newer, ClientOptions{}, "no common protocol version"},
		"other role":        {bulk, ClientOptions{}, `role "bulk"`},
		"check":             {goodHello(), ClientOptions{Check: func(h Hello) error { return errors.New("pod uid " + h.PodUID + " is not the verified pod") }}, "not the verified pod"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, fa, err := dialFake(t, tc.hello, tc.opts, nil)
			var refused *RefusedError
			if !errors.As(err, &refused) || !strings.Contains(refused.Reason, tc.want) {
				t.Fatalf("got %v, want a refusal about %q", err, tc.want)
			}
			if fa == nil || fa.ack.Accept || !strings.Contains(fa.ack.Reason, tc.want) {
				t.Fatalf("the agent got %+v", fa)
			}
		})
	}
}

func TestDialBoundedByContext(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() { _, _ = io.Copy(io.Discard, a) }() // an agent that never says hello
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, b, ClientOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Dial outlived its context")
	}
}

func TestClientLogsAgentRecords(t *testing.T) {
	c := &capture{}
	cl, fa, err := dialFake(t, goodHello(), ClientOptions{Logger: slog.New(c), LogLimit: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"one", "two", "three"} {
		_ = fa.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgLog, Log: &LogRecord{Level: "error", Msg: msg}})
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(c.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = cl.Close()
	recs := c.snapshot()
	if len(recs) != 3 || recs[0].Message != "pod-agent: one" || recs[1].Message != "pod-agent: two" || !strings.Contains(recs[2].Message, "suppressed") {
		var msgs []string
		for _, r := range recs {
			msgs = append(msgs, r.Message)
		}
		t.Fatalf("records %q", msgs)
	}
}

func TestClientClosesASilentSession(t *testing.T) {
	cl, _, err := dialFake(t, goodHello(), ClientOptions{PingInterval: 20 * time.Millisecond, Silence: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cl.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a silent agent's session stayed open")
	}
	if !errors.Is(cl.Err(), ErrAgentSilent) {
		t.Fatalf("err %v", cl.Err())
	}
}

func TestClientHeartbeatKeepsSessionUp(t *testing.T) {
	cl, fa, err := dialFake(t, goodHello(), ClientOptions{PingInterval: 20 * time.Millisecond, Silence: 150 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pings := make(chan struct{}, 100)
	go func() {
		for {
			f, err := fa.ctlR.Next()
			if err != nil {
				return
			}
			if f.Type == FramePing {
				pings <- struct{}{}
				_ = fa.ctlW.Write(FramePing, nil)
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if cl.Err() != nil {
		t.Fatalf("session ended: %v", cl.Err())
	}
	if len(pings) < 5 {
		t.Fatalf("%d pings in 500ms", len(pings))
	}
	if err := cl.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCallCapsTheReply(t *testing.T) {
	handlers := map[Tag]Handler{TagRPC: func(s *Stream) {
		var h RPCHeader
		_ = DecodeHeader(s.Header, &h)
		switch h.Method {
		case "echo":
			_ = s.W.WriteData(FrameData, h.Args)
			_ = s.W.WriteJSON(FrameExit, Result{})
		case "huge":
			chunk := make([]byte, MaxFrame)
			for range MaxReply/MaxFrame + 1 {
				if s.W.Write(FrameData, chunk) != nil {
					return
				}
			}
			_ = s.W.WriteJSON(FrameExit, Result{})
		default:
			_ = s.W.WriteJSON(FrameExit, Result{Code: CodeUnknownMethod, Error: "no rpc " + h.Method})
		}
	}}
	cl, _, err := dialFake(t, goodHello(), ClientOptions{}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var got map[string]int
	if err := cl.Call(ctx, "echo", map[string]int{"n": 7}, &got); err != nil || got["n"] != 7 {
		t.Fatalf("echo: %v %v", got, err)
	}
	if err := cl.Call(ctx, "huge", nil, nil); !errors.Is(err, ErrReplyTooLarge) {
		t.Fatalf("huge: %v", err)
	}
	if err := cl.Call(ctx, "nope", nil, nil); !IsCode(err, CodeUnknownMethod) {
		t.Fatalf("unknown: %v", err)
	}
	// A stream kind the agent does not serve is refused, not hung.
	if _, err := cl.ReadFile(ctx, FileReadHeader{Path: "/etc/hostname"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("unserved kind: %v", err)
	}
}

func TestMuxRefusesAndPenalizes(t *testing.T) {
	old := headerDeadline
	headerDeadline = 100 * time.Millisecond
	t.Cleanup(func() { headerDeadline = old })

	release := make(chan struct{})
	refused := make(chan string, 10)
	a, b := net.Pipe()
	srv, _ := yamux.Server(a, SessionConfig(RoleMain))
	cli, _ := yamux.Client(b, SessionConfig(RoleMain))
	defer cli.Close()
	mux := &Mux{
		Handlers: map[Tag]Handler{TagShareHint: func(*Stream) { <-release }},
		Refused:  func(tag Tag, reason string) { refused <- tag.String() + ": " + reason },
	}
	go func() { _ = mux.Serve(srv) }()
	defer close(release)

	open := func(tag Tag) (*yamux.Stream, Refusal) {
		st, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := WritePreamble(st, tag, struct{}{}); err != nil {
			t.Fatal(err)
		}
		_ = st.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		f, err := NewFrameReader(st).Next()
		var ref Refusal
		if err == nil {
			_ = DecodeFrame(f, &ref)
		}
		return st, ref
	}
	// The kind's cap is 1: the second is refused as busy.
	first, ref := open(TagShareHint)
	if ref.Refused != "" {
		t.Fatalf("first stream refused: %+v", ref)
	}
	defer first.Close()
	if _, ref := open(TagShareHint); ref.Refused != RefusedBusy {
		t.Fatalf("over the cap: %+v", ref)
	}
	if _, ref := open(TagForward); ref.Refused != RefusedUnsupported {
		t.Fatalf("unserved kind: %+v", ref)
	}
	// A stream that never sends its preamble is closed after the deadline,
	// and the session then backs off.
	silent, _ := cli.OpenStream()
	_, _ = silent.Write([]byte{byte(TagRPC)})
	_ = silent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("a stream without a preamble stayed open")
	}
	if mux.penalty.current() < penaltyBase {
		t.Fatalf("no penalty after a broken preamble: %v", mux.penalty.current())
	}
	if len(refused) < 3 {
		t.Fatalf("refusals reported: %d", len(refused))
	}
}

func TestPenaltyBacksOffAndDecays(t *testing.T) {
	var p penalty
	for range 10 {
		p.add()
	}
	if p.current() != penaltyMax {
		t.Fatalf("%v", p.current())
	}
	for range 10 {
		p.clean()
	}
	if p.current() != 0 {
		t.Fatalf("%v after clean streams", p.current())
	}
}

func TestStartErrorFromOpen(t *testing.T) {
	handlers := map[Tag]Handler{TagExec: func(s *Stream) {
		_ = s.W.WriteJSON(FrameOpen, OpenReply{Error: "exec nope: no such file or directory", Code: 127})
	}}
	cl, _, err := dialFake(t, goodHello(), ClientOptions{}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	h := ExecHeader{}
	h.Class, h.Argv = "exec", []string{"nope"}
	_, err = cl.Exec(context.Background(), h)
	var se *StartError
	if !errors.As(err, &se) || se.Code != 127 {
		t.Fatalf("got %v", err)
	}
	raw, _ := json.Marshal(se)
	if !strings.Contains(string(raw), "127") {
		t.Fatalf("%s", raw)
	}
}

// TestClientWaitsForAStreamSlot: fleetd never opens more streams of a kind
// than the agent's cap; one more waits for a slot (within its context).
func TestClientWaitsForAStreamSlot(t *testing.T) {
	release := make(chan struct{})
	served := make(chan struct{}, 64)
	handlers := map[Tag]Handler{TagFileRead: func(s *Stream) {
		served <- struct{}{}
		<-release
		_ = s.W.WriteJSON(FrameOpen, FileInfo{Code: CodeNotFound, Error: "gone"})
	}}
	cl, _, err := dialFake(t, goodHello(), ClientOptions{}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	limit := StreamCap(TagFileRead)
	errs := make(chan error, limit+1)
	for range limit {
		go func() {
			_, err := cl.ReadFile(context.Background(), FileReadHeader{Path: "/x"})
			errs <- err
		}()
	}
	for range limit {
		<-served
	}
	if cl.Active() != limit {
		t.Fatalf("active %d", cl.Active())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := cl.ReadFile(ctx, FileReadHeader{Path: "/x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("over the cap: %v, want a wait that times out", err)
	}
	go func() {
		_, err := cl.ReadFile(context.Background(), FileReadHeader{Path: "/x"})
		errs <- err
	}()
	close(release)
	for range limit + 1 {
		if err := <-errs; !IsCode(err, CodeNotFound) {
			t.Fatalf("got %v", err)
		}
	}
}
