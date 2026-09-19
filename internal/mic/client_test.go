package mic

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeDaemon is a FleetService whose Mic handler the test scripts.
type fakeDaemon struct {
	fleetgrpc.UnimplementedFleetServiceServer
	handler func(fleetgrpc.FleetService_MicServer) error
}

func (d *fakeDaemon) Mic(stream fleetgrpc.FleetService_MicServer) error { return d.handler(stream) }

func dialFakeDaemon(t *testing.T, server fleetgrpc.FleetServiceServer) fleetgrpc.FleetServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, server)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); gs.Stop() })
	return fleetgrpc.NewFleetServiceClient(conn)
}

// stubCapture replaces the recorder with one that emits a frame every few ms,
// counting starts and tracking whether one is running.
type stubCapture struct {
	mu      sync.Mutex
	starts  int
	running int
	devices []string
}

func (s *stubCapture) install(t *testing.T) {
	t.Helper()
	orig := startCapture
	startCapture = func(deviceID string, sink func([]byte)) (*Capture, error) {
		s.mu.Lock()
		s.starts++
		s.running++
		s.devices = append(s.devices, deviceID)
		s.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		capture := &Capture{cancel: cancel, done: make(chan struct{})}
		go func() {
			defer close(capture.done)
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					s.mu.Lock()
					s.running--
					s.mu.Unlock()
					return
				case <-ticker.C:
					sink([]byte("frame"))
				}
			}
		}()
		return capture, nil
	}
	t.Cleanup(func() { startCapture = orig })
}

func (s *stubCapture) snapshot() (starts, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.running
}

type statusLog struct {
	mu   sync.Mutex
	seen []Status
}

func (l *statusLog) report(status Status) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, status)
}

func (l *statusLog) has(state State) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, status := range l.seen {
		if status.State == state {
			return true
		}
	}
	return false
}

func demandFrame(active bool, instances ...string) *fleetgrpc.MicDown {
	return &fleetgrpc.MicDown{Msg: &fleetgrpc.MicDown_Demand{Demand: &fleetgrpc.MicDemand{Active: active, Instances: instances}}}
}

// The privacy contract: the recorder runs only between an active and an
// inactive demand frame, and the audio it produces reaches the daemon.
func TestRunCapturesOnlyWhileDemanded(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)

	opened := make(chan *fleetgrpc.MicOpen, 1)
	gotAudio := make(chan struct{})
	release := make(chan struct{})
	daemon := &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		first, err := stream.Recv()
		if err != nil {
			return err
		}
		opened <- first.GetOpen()
		_ = stream.Send(demandFrame(false))
		// Idle: give a wrongly-eager client the chance to start capturing.
		time.Sleep(50 * time.Millisecond)
		if starts, _ := recorder.snapshot(); starts != 0 {
			t.Errorf("recorder started %d times with no demand", starts)
		}
		_ = stream.Send(demandFrame(true, "alpha/i1"))
		for {
			up, err := stream.Recv()
			if err != nil {
				return err
			}
			if string(up.GetAudio()) == "frame" {
				break
			}
		}
		close(gotAudio)
		_ = stream.Send(demandFrame(false))
		<-release
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	done := make(chan struct{})
	go func() {
		Run(ctx, dialFakeDaemon(t, daemon), func() string { return "pulse:yeti" }, log.report)
		close(done)
	}()

	select {
	case open := <-opened:
		if open.GetSampleRate() != SampleRate || open.GetChannels() != Channels {
			t.Fatalf("open header = %v", open)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no open header")
	}
	select {
	case <-gotAudio:
	case <-time.After(5 * time.Second):
		t.Fatal("no audio reached the daemon")
	}
	waitFor(t, "the recorder to stop when demand ends", func() bool {
		_, running := recorder.snapshot()
		return running == 0
	})
	if starts, _ := recorder.snapshot(); starts != 1 {
		t.Fatalf("recorder started %d times, want 1", starts)
	}
	if recorder.devices[0] != "pulse:yeti" {
		t.Fatalf("recorded from %q, want the configured device", recorder.devices[0])
	}
	if !log.has(StateIdle) || !log.has(StateLive) {
		t.Fatalf("status log missing idle/live: %+v", log.seen)
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A stream that drops mid-recording must release the microphone.
func TestRunStopsCaptureWhenTheStreamDrops(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)
	var calls int
	var mu sync.Mutex
	daemon := &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if !first {
			_ = stream.Send(demandFrame(false))
			<-stream.Context().Done()
			return nil
		}
		_ = stream.Send(demandFrame(true, "alpha/i1"))
		if _, err := stream.Recv(); err != nil { // wait for audio, then die
			return err
		}
		return status.Error(codes.Unavailable, "daemon restarting")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, daemon), func() string { return "" }, log.report)

	waitFor(t, "a reconnect", func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 })
	waitFor(t, "the recorder to be released", func() bool {
		starts, running := recorder.snapshot()
		return starts == 1 && running == 0
	})
}

func TestRunGivesUpOnADaemonWithoutTheRPC(t *testing.T) {
	var log statusLog
	done := make(chan struct{})
	go func() {
		Run(context.Background(), dialFakeDaemon(t, &fleetgrpc.UnimplementedFleetServiceServer{}), func() string { return "" }, log.report)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return when the daemon predates the Mic RPC")
	}
	if !log.has(StateUnsupported) {
		t.Fatalf("status log = %+v, want StateUnsupported", log.seen)
	}
}

func TestRunReportsADisabledDaemon(t *testing.T) {
	daemon := &fakeDaemon{handler: func(fleetgrpc.FleetService_MicServer) error {
		return status.Error(codes.FailedPrecondition, "the microphone is disabled in settings")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, daemon), func() string { return "" }, log.report)
	waitFor(t, "StateDisabled", func() bool { return log.has(StateDisabled) })
}
