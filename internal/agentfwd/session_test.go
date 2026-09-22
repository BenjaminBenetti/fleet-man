package agentfwd

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentproto"
)

const (
	agentcRequestIdentities = 11
	agentcExtension         = 27
)

// agentMessage frames body as an agent-protocol message.
func agentMessage(body ...byte) []byte {
	msg := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(msg, uint32(len(body)))
	return append(msg, body...)
}

// recordingAgent is a minimal agent that records the request types it
// receives, counts live connections, and answers REQUEST_IDENTITIES with an
// empty list (everything else with FAILURE).
type recordingAgent struct {
	mu    sync.Mutex
	types []byte
	live  atomic.Int32
	total atomic.Int32
	// delay holds each reply back (a slow agent).
	delay time.Duration
}

func startRecordingAgent(t *testing.T) *recordingAgent {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "afr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	a := &recordingAgent{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			a.live.Add(1)
			a.total.Add(1)
			go func() {
				defer a.live.Add(-1)
				defer conn.Close()
				for {
					msg, err := agentproto.ReadMessage(conn)
					if err != nil {
						return
					}
					a.mu.Lock()
					a.types = append(a.types, msg[4])
					a.mu.Unlock()
					time.Sleep(a.delay)
					reply := agentproto.FailureReply
					if msg[4] == agentcRequestIdentities {
						reply = []byte{0, 0, 0, 5, 12, 0, 0, 0, 0}
					}
					if _, err := conn.Write(reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	setAgentSocket(t, sock)
	return a
}

func (a *recordingAgent) seen() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.types...)
}

// scriptedDaemon hands the test the raw server stream.
func scriptedDaemon(t *testing.T) (fleetgrpc.FleetServiceClient, <-chan fleetgrpc.FleetService_SSHAgentServer) {
	t.Helper()
	streams := make(chan fleetgrpc.FleetService_SSHAgentServer, 1)
	client := dialFake(t, &fakeDaemon{handle: func(stream fleetgrpc.FleetService_SSHAgentServer) error {
		if _, err := stream.Recv(); err != nil { // hello
			return err
		}
		streams <- stream
		<-stream.Context().Done()
		return nil
	}})
	return client, streams
}

func runProvider(t *testing.T, client fleetgrpc.FleetServiceClient) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Run(ctx, client, "test", func(Status) {})
}

func recvUp(t *testing.T, stream fleetgrpc.FleetService_SSHAgentServer) *fleetgrpc.SSHAgentUp {
	t.Helper()
	type result struct {
		up  *fleetgrpc.SSHAgentUp
		err error
	}
	got := make(chan result, 1)
	go func() {
		up, err := stream.Recv()
		got <- result{up, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("recv: %v", r.err)
		}
		return r.up
	case <-time.After(10 * time.Second):
		t.Fatal("no frame from the provider")
		return nil
	}
}

func sendDown(t *testing.T, stream fleetgrpc.FleetService_SSHAgentServer, msg *fleetgrpc.SSHAgentDown) {
	t.Helper()
	if err := stream.Send(msg); err != nil {
		t.Fatal(err)
	}
}

func downOpen(id uint64) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Open{Open: &fleetgrpc.SSHAgentOpen{ConnId: id}}}
}

func downData(id uint64, data []byte) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Data{Data: &fleetgrpc.SSHAgentData{ConnId: id, Data: data}}}
}

func downClose(id uint64) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Close{Close: &fleetgrpc.SSHAgentClose{ConnId: id}}}
}

func TestProviderAnswersForbiddenRequestsItself(t *testing.T) {
	agent := startRecordingAgent(t)
	client, streams := scriptedDaemon(t)
	runProvider(t, client)
	stream := <-streams

	sendDown(t, stream, downOpen(1))
	if up := recvUp(t, stream); up.GetReady().GetConnId() != 1 {
		t.Fatalf("want ready, got %v", up)
	}
	// SSH_AGENTC_ADD_SMARTCARD_KEY naming a provider library: what a remote
	// process would send to make the user's agent dlopen() something.
	addSmartcard := agentMessage(append([]byte{20, 0, 0, 0, 16}, "/usr/lib/evil.so"...)...)
	// One frame carrying the forbidden request AND a list request after it:
	// the replies must come back in order.
	sendDown(t, stream, downData(1, append(addSmartcard, agentMessage(agentcRequestIdentities)...)))

	var replies []byte
	for len(replies) < len(agentproto.FailureReply)+9 {
		up := recvUp(t, stream)
		replies = append(replies, up.GetData().GetData()...)
	}
	want := append(append([]byte(nil), agentproto.FailureReply...), 0, 0, 0, 5, 12, 0, 0, 0, 0)
	if !bytes.Equal(replies, want) {
		t.Fatalf("replies = %v, want FAILURE then an identities answer %v", replies, want)
	}
	// The provider's own forwarding bind (an extension) comes first; of the
	// daemon's requests only the list may reach the agent.
	if seen := agent.seen(); !bytes.Equal(seen, []byte{agentcExtension, agentcRequestIdentities}) {
		t.Fatalf("the agent saw request types %v; want the bind, then only the list request", seen)
	}
}

func TestProviderDoesNotLeakConnectionsTheDaemonAbandoned(t *testing.T) {
	agent := startRecordingAgent(t)
	client, streams := scriptedDaemon(t)
	runProvider(t, client)
	stream := <-streams

	// The daemon gave up waiting (its ready timeout) and closes right behind
	// every open — the close must not be lost while the provider dials.
	base := agent.total.Load() // the reachability probe before attaching
	for id := uint64(1); id <= 20; id++ {
		sendDown(t, stream, downOpen(id))
		sendDown(t, stream, downClose(id))
	}
	// A ping round trip proves the provider's receive loop has handled every
	// open and close above.
	sendDown(t, stream, &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Ping{Ping: &fleetgrpc.SSHAgentPing{Seq: 1}}})
	for {
		if up := recvUp(t, stream); up.GetPong() != nil {
			break
		}
	}
	// Every open was dialed (the provider cannot know it was abandoned until
	// it looks), and every one of those agent connections must be closed.
	deadline := time.Now().Add(5 * time.Second)
	for (agent.total.Load()-base < 20 || agent.live.Load() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dialed := agent.total.Load() - base; dialed < 20 {
		t.Fatalf("only %d of the 20 opens reached the agent; the test did not exercise the race", dialed)
	}
	if n := agent.live.Load(); n != 0 {
		t.Fatalf("%d agent connections left open for connections the daemon abandoned", n)
	}
}

func TestProviderStillRepliesAfterTheDaemonHalfCloses(t *testing.T) {
	startRecordingAgent(t)
	client, streams := scriptedDaemon(t)
	runProvider(t, client)
	stream := <-streams

	sendDown(t, stream, downOpen(5))
	recvUp(t, stream) // ready
	// A client that sends its request and shuts its write side at once.
	sendDown(t, stream, downData(5, agentMessage(agentcRequestIdentities)))
	sendDown(t, stream, downClose(5))

	up := recvUp(t, stream)
	if !bytes.Equal(up.GetData().GetData(), []byte{0, 0, 0, 5, 12, 0, 0, 0, 0}) {
		t.Fatalf("want the reply to the request sent before the close, got %v", up)
	}
	if up := recvUp(t, stream); up.GetClose().GetConnId() != 5 {
		t.Fatalf("want the provider's close after the reply, got %v", up)
	}
}

func TestProviderAnswersPings(t *testing.T) {
	startRecordingAgent(t)
	client, streams := scriptedDaemon(t)
	runProvider(t, client)
	stream := <-streams

	sendDown(t, stream, &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Ping{Ping: &fleetgrpc.SSHAgentPing{Seq: 42}}})
	if up := recvUp(t, stream); up.GetPong().GetSeq() != 42 {
		t.Fatalf("want pong 42, got %v", up)
	}
}

func TestProviderAnswersRequestsQueuedBeforeTheDaemonsClose(t *testing.T) {
	agent := startRecordingAgent(t)
	agent.delay = 30 * time.Millisecond // requests pile up behind a slow agent
	client, streams := scriptedDaemon(t)
	runProvider(t, client)
	stream := <-streams

	answer := []byte{0, 0, 0, 5, 12, 0, 0, 0, 0}
	for id := uint64(1); id <= 10; id++ {
		sendDown(t, stream, downOpen(id))
		for {
			if up := recvUp(t, stream); up.GetReady().GetConnId() == id {
				break
			}
		}
		// Two requests in separate frames, then the client's half-close, all
		// before the agent has answered the first.
		sendDown(t, stream, downData(id, agentMessage(agentcRequestIdentities)))
		sendDown(t, stream, downData(id, agentMessage(agentcRequestIdentities)))
		sendDown(t, stream, downClose(id))

		var got []byte
		for {
			up := recvUp(t, stream)
			if c := up.GetClose(); c != nil && c.GetConnId() == id {
				break
			}
			if d := up.GetData(); d.GetConnId() == id {
				got = append(got, d.GetData()...)
			}
		}
		if !bytes.Equal(got, append(append([]byte(nil), answer...), answer...)) {
			t.Fatalf("conn %d: replies before the provider's close = %v, want two identity answers", id, got)
		}
	}
}
