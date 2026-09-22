package agentfwd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeDaemon implements just the SSHAgent RPC, driven by the test.
type fakeDaemon struct {
	fleetgrpc.UnimplementedFleetServiceServer
	handle func(fleetgrpc.FleetService_SSHAgentServer) error
}

func (d *fakeDaemon) SSHAgent(stream fleetgrpc.FleetService_SSHAgentServer) error {
	return d.handle(stream)
}

// dialFake serves srv over an in-memory listener and returns a client.
func dialFake(t *testing.T, srv fleetgrpc.FleetServiceServer) fleetgrpc.FleetServiceClient {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, srv)
	go func() { _ = gs.Serve(ln) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		gs.Stop()
	})
	return fleetgrpc.NewFleetServiceClient(conn)
}

// localAgent serves an in-process agent with one key and points the
// agentSocket seam at it.
func localAgent(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "afw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: "user"}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	setAgentSocket(t, sock)
}

func setAgentSocket(t *testing.T, sock string) {
	t.Helper()
	orig := agentSocket
	t.Cleanup(func() { agentSocket = orig })
	agentSocket = func() string { return sock }
}

// reports collects Run's status reports.
type reports chan Status

func (r reports) report(st Status) {
	select {
	case r <- st:
	default:
	}
}

func (r reports) waitFor(t *testing.T, want State) Status {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case st := <-r:
			if st.State == want {
				return st
			}
		case <-deadline:
			t.Fatalf("never reached state %v", want)
		}
	}
}

func TestRunStaysDetachedWithoutALocalAgent(t *testing.T) {
	setAgentSocket(t, "")
	attached := make(chan struct{}, 1)
	client := dialFake(t, &fakeDaemon{handle: func(fleetgrpc.FleetService_SSHAgentServer) error {
		attached <- struct{}{}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(reports, 16)
	go Run(ctx, client, "test", got.report)
	st := got.waitFor(t, StateNoAgent)
	if !strings.Contains(st.Detail, "SSH_AUTH_SOCK") {
		t.Fatalf("detail = %q", st.Detail)
	}
	select {
	case <-attached:
		t.Fatal("a provider with no agent must not attach (it would shadow the daemon's own agent)")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRunStopsOnAnOldDaemon(t *testing.T) {
	localAgent(t)
	client := dialFake(t, &fleetgrpc.UnimplementedFleetServiceServer{})
	got := make(reports, 16)
	done := make(chan struct{})
	go func() {
		Run(context.Background(), client, "test", got.report)
		close(done)
	}()
	got.waitFor(t, StateUnsupported)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run should return against a daemon without the RPC")
	}
}

func TestRunReportsARefusal(t *testing.T) {
	localAgent(t)
	client := dialFake(t, &fakeDaemon{handle: func(fleetgrpc.FleetService_SSHAgentServer) error {
		return status.Error(codes.FailedPrecondition, "SSH agent forwarding is turned off on this host")
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(reports, 16)
	go Run(ctx, client, "test", got.report)
	if st := got.waitFor(t, StateRefused); !strings.Contains(st.Detail, "turned off") {
		t.Fatalf("detail = %q", st.Detail)
	}
}

// agentRequestIdentities is an agent-protocol SSH_AGENTC_REQUEST_IDENTITIES
// message: a 4-byte length, then the type byte 11.
var agentRequestIdentities = []byte{0, 0, 0, 1, 11}

func TestRunSplicesAConnectionOntoTheLocalAgent(t *testing.T) {
	localAgent(t)
	type exchange struct {
		reply []byte
		err   error
	}
	result := make(chan exchange, 1)
	client := dialFake(t, &fakeDaemon{handle: func(stream fleetgrpc.FleetService_SSHAgentServer) error {
		fail := func(err error) error { result <- exchange{err: err}; return err }
		if first, err := stream.Recv(); err != nil || first.GetHello() == nil {
			return fail(status.Error(codes.InvalidArgument, "want hello first"))
		}
		_ = stream.Send(&fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Status{Status: &fleetgrpc.SSHAgentStatus{Active: true}}})
		_ = stream.Send(&fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Open{Open: &fleetgrpc.SSHAgentOpen{ConnId: 7, Origin: "f/i"}}})
		up, err := stream.Recv()
		if err != nil || up.GetReady().GetConnId() != 7 {
			return fail(status.Errorf(codes.Internal, "want ready for 7, got %v (%v)", up, err))
		}
		_ = stream.Send(&fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Data{Data: &fleetgrpc.SSHAgentData{ConnId: 7, Data: agentRequestIdentities}}})
		var reply []byte
		for {
			up, err := stream.Recv()
			if err != nil {
				return fail(err)
			}
			reply = append(reply, up.GetData().GetData()...)
			if len(reply) >= 4 && len(reply) >= 4+int(binary.BigEndian.Uint32(reply)) {
				break
			}
		}
		_ = stream.Send(&fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Close{Close: &fleetgrpc.SSHAgentClose{ConnId: 7}}})
		result <- exchange{reply: reply}
		<-stream.Context().Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(reports, 64)
	go Run(ctx, client, "test", got.report)

	got.waitFor(t, StateActive)
	var ex exchange
	select {
	case ex = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("no exchange")
	}
	if ex.err != nil {
		t.Fatal(ex.err)
	}
	// SSH_AGENT_IDENTITIES_ANSWER (12) with exactly one key.
	if len(ex.reply) < 9 || ex.reply[4] != 12 || binary.BigEndian.Uint32(ex.reply[5:9]) != 1 {
		t.Fatalf("reply %v is not an identities answer with one key", ex.reply)
	}
	if st := got.waitFor(t, StateActive); st.Uses < 1 {
		// The use is reported after the ready; drain until it shows.
		for st.Uses < 1 {
			st = got.waitFor(t, StateActive)
		}
	}
}

func TestRunTellsTheDaemonWhenTheLocalAgentIsGone(t *testing.T) {
	localAgent(t)
	live := agentSocket()
	var gone atomic.Bool
	setAgentSocket(t, "")
	agentSocket = func() string {
		if gone.Load() {
			return filepath.Join(os.TempDir(), "no-such-agent.sock")
		}
		return live
	}
	gotClose := make(chan *fleetgrpc.SSHAgentClose, 1)
	client := dialFake(t, &fakeDaemon{handle: func(stream fleetgrpc.FleetService_SSHAgentServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		// The agent disappears after the provider attached.
		gone.Store(true)
		_ = stream.Send(&fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Open{Open: &fleetgrpc.SSHAgentOpen{ConnId: 3}}})
		up, err := stream.Recv()
		if err != nil {
			return err
		}
		gotClose <- up.GetClose()
		<-stream.Context().Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, client, "test", func(Status) {})
	select {
	case c := <-gotClose:
		if c.GetConnId() != 3 || c.GetError() == "" {
			t.Fatalf("want a close with an error for conn 3 (so the daemon falls back), got %v", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to the open")
	}
}
