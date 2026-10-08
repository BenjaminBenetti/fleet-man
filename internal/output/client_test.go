package output

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type outputTestDaemon struct {
	fleetgrpc.UnimplementedFleetServiceServer
	commands chan *fleetgrpc.OutputDown
}

func (d *outputTestDaemon) Output(s fleetgrpc.FleetService_OutputServer) error {
	if _, err := s.Recv(); err != nil {
		return err
	}
	if _, err := s.Recv(); err != nil {
		return err
	}
	for {
		select {
		case msg := <-d.commands:
			if err := s.Send(msg); err != nil {
				return err
			}
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

func TestClientSwitchStopsPlayerAndCancellationDoesNotWaitForAudioDrain(t *testing.T) {
	d := &outputTestDaemon{commands: make(chan *fleetgrpc.OutputDown, 32)}
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(server, d)
	go server.Serve(lis)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///output", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	originalList := listDevices
	listings := make(chan []Device, 4)
	listDevices = func() (string, []Device, error) { return "pulse", <-listings, nil }
	defer func() { listDevices = originalList }()
	type started struct {
		p    *player
		argv []string
	}
	starts := make(chan started, 8)
	original := startPlayer
	startPlayer = func(ctx context.Context, argv []string) (*player, error) {
		ctx, cancel := context.WithCancel(ctx)
		p := &player{frames: make(chan []byte, 1), done: make(chan struct{}), cancel: cancel, stdin: discardCloser{}}
		// Never drain frames: a hung player must not hold selection or shutdown.
		go func() { <-ctx.Done(); close(p.done) }()
		starts <- started{p, argv}
		return p, nil
	}
	defer func() { startPlayer = original }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	statuses := make(chan Status, 32)
	go func() {
		done <- runStream(ctx, fleetgrpc.NewFleetServiceClient(conn), "pulse", []Device{{ID: "pulse:first"}, {ID: "pulse:second"}}, func(s Status) { statuses <- s })
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("client did not stop")
		}
	}()
	selectDevice := func(active bool, device string) {
		d.commands <- &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_Selection{Selection: &fleetgrpc.OutputSelection{Active: active, Device: device}}}
	}
	feed := func() {
		for i := 0; i < 8; i++ {
			d.commands <- &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_Audio{Audio: &fleetgrpc.OutputAudio{Instance: "f/i", Pcm: bytes.Repeat([]byte{1, 2, 3, 4}, ChunkBytes/4)}}}
		}
	}
	next := func() started {
		select {
		case p := <-starts:
			return p
		case <-time.After(time.Second):
			t.Fatal("player not started")
			return started{}
		}
	}
	selectDevice(true, "pulse:first")
	feed()
	first := next()
	if !strings.Contains(strings.Join(first.argv, " "), "--device=first") {
		t.Fatalf("wrong first device: %v", first.argv)
	}
	selectDevice(true, "pulse:second")
	feed()
	second := next()
	select {
	case <-first.p.done:
	default:
		t.Fatal("old player survived device change")
	}
	if !strings.Contains(strings.Join(second.argv, " "), "--device=second") {
		t.Fatalf("wrong second device: %v", second.argv)
	}
	// Emulate a device that vanished after enumeration. The default is used
	// immediately, but a fresh listing must restore the retained selection.
	second.p.cancel()
	select {
	case <-second.p.done:
	case <-time.After(time.Second):
		t.Fatal("failed player did not exit")
	}
	deadline := time.After(time.Second)
waitForFallback:
	for {
		select {
		case s := <-statuses:
			if s.Detail == "device unavailable — using system default" {
				break waitForFallback
			}
		case <-deadline:
			t.Fatal("failed device did not trigger fallback")
		}
	}
	feed()
	fallback := next()
	if strings.Contains(strings.Join(fallback.argv, " "), "--device=") {
		t.Fatalf("failed device did not fall back: %v", fallback.argv)
	}
	relist := func(ds []Device) {
		listings <- ds
		d.commands <- &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_ListDevices{ListDevices: &fleetgrpc.OutputListDevices{}}}
	}
	relist([]Device{{ID: "pulse:first"}, {ID: "pulse:second"}})
	select {
	case <-fallback.p.done:
	case <-time.After(time.Second):
		t.Fatal("fresh enumeration did not restore requested device")
	}
	feed()
	second = next()
	if !strings.Contains(strings.Join(second.argv, " "), "--device=second") {
		t.Fatalf("requested device was forgotten: %v", second.argv)
	}
	// Hot removal must also move a player that has not exited on its own.
	relist([]Device{{ID: "pulse:first"}})
	select {
	case <-second.p.done:
	case <-time.After(time.Second):
		t.Fatal("removed device remained selected")
	}
	feed()
	second = next()
	if strings.Contains(strings.Join(second.argv, " "), "--device=") {
		t.Fatalf("missing device did not fall back: %v", second.argv)
	}
	selectDevice(false, "")
	select {
	case <-second.p.done:
	case <-time.After(time.Second):
		t.Fatal("standby client kept playing")
	}
	selectDevice(true, "pulse:first")
	feed()
	third := next()
	cancel()
	select {
	case <-third.p.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for stalled playback")
	}
}

func TestClientReleasesSelectionWhenOutputsDisappear(t *testing.T) {
	d := &outputTestDaemon{commands: make(chan *fleetgrpc.OutputDown, 4)}
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(server, d)
	go server.Serve(lis)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///output", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	original := listDevices
	want := errors.New("no playback devices")
	listDevices = func() (string, []Device, error) { return "", nil, want }
	defer func() { listDevices = original }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.commands <- &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_ListDevices{ListDevices: &fleetgrpc.OutputListDevices{}}}
	if err := runStream(ctx, fleetgrpc.NewFleetServiceClient(conn), "pulse", []Device{{ID: "pulse:usb"}}, func(Status) {}); !errors.Is(err, want) {
		t.Fatalf("unavailable client held selection: %v", err)
	}
}

func TestPlayerStopUnblocksFullStdin(t *testing.T) {
	p, err := startPlayer(context.Background(), []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		p.write(bytes.Repeat([]byte{1}, 64*1024))
	}
	done := make(chan struct{})
	go func() { p.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopping player blocked behind its full stdin")
	}
}
