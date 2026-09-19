package mic

import (
	"context"
	"errors"
	"net"
	"strings"
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
	// failFirst makes that many leading starts fail with failWith.
	failFirst int
	failWith  error
	// frame is what the fake recorder emits each tick (default "frame").
	frame []byte
}

func (s *stubCapture) install(t *testing.T) {
	t.Helper()
	orig := startCapture
	startCapture = func(deviceID string, sink func([]byte), _ func()) (*Capture, error) {
		s.mu.Lock()
		s.starts++
		s.devices = append(s.devices, deviceID)
		if s.starts <= s.failFirst {
			err := s.failWith
			s.mu.Unlock()
			return nil, err
		}
		s.running++
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
					if s.frame != nil {
						sink(s.frame)
					} else {
						sink([]byte("frame"))
					}
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

// holdDemand is a daemon that reports demand and then just keeps the stream up.
func holdDemand() *fakeDaemon {
	return &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		_ = stream.Send(demandFrame(true, "alpha/i1"))
		<-stream.Context().Done()
		return nil
	}}
}

// A recorder that cannot start (device busy) is retried while demand lasts —
// and the device is re-read each time, so a selection changed in Settings
// applies to the very next attempt without restarting the provider.
func TestRunRetriesAFailedCaptureWithTheCurrentDevice(t *testing.T) {
	recorder := stubCapture{failFirst: 1, failWith: errors.New("device busy")}
	recorder.install(t)

	var mu sync.Mutex
	device := "pulse:busy"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, holdDemand()), func() string {
		mu.Lock()
		defer mu.Unlock()
		return device
	}, func(status Status) {
		log.report(status)
		if status.State == StateError {
			mu.Lock()
			device = "pulse:other" // the user picks another device meanwhile
			mu.Unlock()
		}
	})

	waitFor(t, "the retry to succeed", func() bool { _, running := recorder.snapshot(); return running == 1 })
	recorder.mu.Lock()
	devices := append([]string(nil), recorder.devices...)
	recorder.mu.Unlock()
	if len(devices) != 2 || devices[0] != "pulse:busy" || devices[1] != "pulse:other" {
		t.Fatalf("capture attempts used %v, want [pulse:busy pulse:other]", devices)
	}
	if !log.has(StateError) || !log.has(StateLive) {
		t.Fatalf("status log missing error/live: %+v", log.seen)
	}
}

// No recorder on this machine is not something a retry fixes: report it once,
// and do not spin.
func TestRunDoesNotRetryWithoutACaptureTool(t *testing.T) {
	recorder := stubCapture{failFirst: 1000, failWith: ErrNoCaptureTool}
	recorder.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, holdDemand()), func() string { return "" }, log.report)

	waitFor(t, "the error report", func() bool { return log.has(StateError) })
	time.Sleep(captureRetry + 200*time.Millisecond)
	if starts, _ := recorder.snapshot(); starts != 1 {
		t.Fatalf("capture was attempted %d times; a missing tool must not be retried", starts)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, status := range log.seen {
		if status.State == StateError && !strings.Contains(status.Detail, "install") {
			t.Fatalf("the error should carry the install hint: %q", status.Detail)
		}
	}
}

// Closing the real microphone must never wait on the network. A daemon that has
// stopped reading stalls Send on flow control; if audio were sent from the
// recorder's own loop, Capture.Stop would wait on that Send and the microphone
// would stay open for as long as the peer stays stalled.
func TestRunClosesTheMicEvenWhenTheNetworkIsStalled(t *testing.T) {
	recorder := stubCapture{frame: make([]byte, 64*1024)} // fills the window fast
	recorder.install(t)

	stalled := make(chan struct{})
	daemon := &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		if _, err := stream.Recv(); err != nil { // the open header — then never read again
			return err
		}
		_ = stream.Send(demandFrame(true, "alpha/i1"))
		<-stalled
		_ = stream.Send(demandFrame(false))
		<-stream.Context().Done()
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, daemon), func() string { return "" }, log.report)

	waitFor(t, "the recorder to start", func() bool { _, running := recorder.snapshot(); return running == 1 })
	time.Sleep(300 * time.Millisecond) // let the send path wedge on flow control
	close(stalled)
	waitFor(t, "the recorder to be released despite the stalled stream", func() bool {
		_, running := recorder.snapshot()
		return running == 0
	})
}

// A recorder that keeps failing is retried with a growing delay, not once a
// second for as long as someone holds the talk key.
func TestRunBacksOffAFailingRecorder(t *testing.T) {
	recorder := stubCapture{failFirst: 1000, failWith: errors.New("device busy")}
	recorder.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, holdDemand()), func() string { return "" }, log.report)

	// 1 s, then 2 s: three attempts need ~3 s. A flat 1 s retry would make four
	// or more in 3.5 s.
	time.Sleep(3500 * time.Millisecond)
	if starts, _ := recorder.snapshot(); starts < 2 || starts > 3 {
		t.Fatalf("%d capture attempts in 3.5 s; want 2-3 (1 s then 2 s back-off)", starts)
	}
}

// The send queue has TWO consumers: the sender goroutine and the drain that
// runs when a recording ends. A drain that trusts len() parks forever the first
// time the sender takes the last frame between the len() and the receive — and
// with it the whole provider loop. The interleaving is rare per attempt (the
// reviewer who found it hit it on trial 151 of 20 000), so: many attempts.
func TestFrameQueueDrainNeverBlocksAgainstACompetingConsumer(t *testing.T) {
	for trial := range 20000 {
		queue := make(frameQueue, sendQueue)
		for range 4 {
			queue.push([]byte("pcm"))
		}
		go func() { // the sender goroutine, mid-recording
			for range 4 {
				select {
				case <-queue:
				default:
				}
			}
		}()
		drained := make(chan struct{})
		go func() { queue.drain(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatalf("drain blocked on trial %d", trial)
		}
	}
}

func TestFrameQueueDropsWhenFull(t *testing.T) {
	queue := make(frameQueue, 2)
	done := make(chan struct{})
	go func() {
		for range 10 {
			queue.push([]byte("pcm"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push blocked on a full queue")
	}
	if len(queue) != 2 {
		t.Fatalf("queue holds %d frames, want 2 (the rest dropped)", len(queue))
	}
}

// A liveness smoke test over the real stream: demand flapping against a slow-
// but-alive peer must leave the loop still answering. (It does not reliably hit
// the drain race above — that needs the tight loop — but it exercises stop()
// with audio genuinely in flight.)
func TestRunSurvivesDemandFlappingAgainstASlowPeer(t *testing.T) {
	// Big frames, so the slow reader stalls Send on flow control and the queue
	// genuinely backs up.
	recorder := stubCapture{frame: make([]byte, 48*1024)}
	recorder.install(t)

	const rounds = 150
	finished := make(chan struct{})
	daemon := &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		go func() { // slow-but-alive reader
			for {
				if _, err := stream.Recv(); err != nil {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		for range rounds {
			_ = stream.Send(demandFrame(true, "alpha/i1"))
			time.Sleep(8 * time.Millisecond) // long enough for the queue to back up
			_ = stream.Send(demandFrame(false))
			time.Sleep(2 * time.Millisecond)
		}
		close(finished)
		<-stream.Context().Done()
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log statusLog
	go Run(ctx, dialFakeDaemon(t, daemon), func() string { return "" }, log.report)

	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never finished flapping demand")
	}
	// If stop() had parked, the final "inactive" would never be acted on.
	waitFor(t, "the loop to still be handling demand", func() bool {
		starts, running := recorder.snapshot()
		return starts > rounds/2 && running == 0
	})
}
