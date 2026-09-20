package mic

import (
	"context"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// State is the microphone provider's coarse condition, for the UI.
type State int

const (
	// StateConnecting: no Mic stream to the daemon yet (or it dropped).
	StateConnecting State = iota
	// StateIdle: attached, nothing in any instance is recording. The real
	// microphone is CLOSED.
	StateIdle
	// StateLive: the real microphone is open and streaming into Instances.
	StateLive
	// StateError: attached, but capture failed (Detail says why). Retried while
	// demand persists.
	StateError
	// StateUnsupported: the daemon does not know the Mic RPC (older fleetd).
	// Terminal — Run returns.
	StateUnsupported
	// StateDisabled: the daemon has the microphone turned off. Retried slowly,
	// since the setting may be flipped from another client.
	StateDisabled
)

// Status is one provider status report.
type Status struct {
	State State
	// Instances names who is recording ("<fleet>/<instance>") while live.
	Instances []string
	// Detail carries the error text for StateError.
	Detail string
	// FellBack is set while live if the configured device is not on this
	// machine and the system default is being recorded instead.
	FellBack bool
}

const (
	reconnectInitial = 500 * time.Millisecond
	reconnectMax     = 10 * time.Second
	disabledRetry    = 5 * time.Second
	// captureRetry / captureRetryMax pace restarts of a recorder that will not
	// start or keeps dying while demand lasts: quick at first (a device busy for
	// a moment), backing off so a permanently broken one is not respawned every
	// second for as long as someone holds the talk key.
	captureRetry    = time.Second
	captureRetryMax = 30 * time.Second
	// captureHealthy is how long a recorder must stay up before the back-off is
	// forgiven. "It started" proves nothing — that only means a process was
	// spawned; a recorder that dies the instant it opens the device would
	// otherwise be respawned at a flat 1 Hz for as long as demand lasts.
	captureHealthy = 5 * time.Second
	// sendQueue bounds captured audio waiting for the network, in frames
	// (~40 ms each). Late audio is worthless, so a full queue DROPS — the same
	// policy the daemon applies toward its sinks.
	sendQueue = 25
)

// frameQueue carries captured audio from the recorder's read loop to the
// stream's sender goroutine. Both operations are NON-BLOCKING by construction:
// push drops when the network is behind, and drain — which shares the channel
// with the sender as a second consumer — never does a bare receive. (A drain
// written as `for len(q) > 0 { <-q }` parks forever the first time the sender
// takes the last frame between the len() and the receive.)
type frameQueue chan []byte

func (q frameQueue) push(pcm []byte) {
	select {
	case q <- pcm:
	default:
	}
}

func (q frameQueue) drain() {
	for {
		select {
		case <-q:
		default:
			return
		}
	}
}

// startCapture is a seam so tests can run the stream logic with no recorder.
var startCapture = StartNotify

// Run is the microphone provider loop: it holds a Mic stream to the daemon,
// opens the real microphone only while the daemon reports demand, and streams
// the captured PCM up. It reconnects with backoff and returns when ctx is
// cancelled (or the daemon turns out not to support the RPC).
//
// The device to record from is PUSHED by the daemon with every demand (it owns
// the config), so nothing is looked up on the capture-start path and a changed
// selection applies to the next recording. override, if non-empty, wins over it
// (`fleet mic attach --device`). report is called from Run's goroutine on every
// status change; it must not block.
func Run(ctx context.Context, svc fleetgrpc.FleetServiceClient, override string, report func(Status)) {
	backoff := reconnectInitial
	disabled := false
	for ctx.Err() == nil {
		// A disabled daemon is polled quietly: flipping the status back to
		// "connecting" on every retry would just make the read-out flicker.
		if !disabled {
			report(Status{State: StateConnecting})
		}
		attached, err := runStream(ctx, svc, override, report)
		disabled = false
		wait := backoff
		switch status.Code(err) {
		case codes.Unimplemented:
			report(Status{State: StateUnsupported})
			return
		case codes.FailedPrecondition:
			report(Status{State: StateDisabled})
			disabled = true
			wait = disabledRetry
		default:
			if attached {
				backoff = reconnectInitial
				wait = backoff
			} else {
				backoff = min(backoff*2, reconnectMax)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// runStream runs one Mic stream to completion. attached reports whether the
// daemon accepted the stream (so the caller resets its backoff).
func runStream(ctx context.Context, svc fleetgrpc.FleetServiceClient, override string, report func(Status)) (attached bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := svc.Mic(ctx, grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
	if err := stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{
		SampleRate: SampleRate,
		Channels:   Channels,
	}}}); err != nil {
		return false, err
	}

	demands := make(chan *fleetgrpc.MicDemand)
	recvErr := make(chan error, 1)
	go func() {
		for {
			down, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if demand := down.GetDemand(); demand != nil {
				select {
				case demands <- demand:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Captured audio reaches the stream through a queue and ONE sender goroutine
	// for the life of the stream — never from the recorder's read loop. Two
	// reasons: Stream.Send is not safe for concurrent use, and a Send stalled on
	// the network must not be what Capture.Stop waits for. Closing the real
	// microphone is the one thing here that has to be prompt.
	frames := make(frameQueue, sendQueue)
	go func() {
		for {
			select {
			case pcm := <-frames:
				if stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Audio{Audio: pcm}}) != nil {
					return // the recv loop reports the stream's death
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	fellBack := make(chan struct{}, 1)

	var (
		capture    *Capture
		captureCh  <-chan struct{} // capture.Done() while a capture is running
		retry      <-chan time.Time
		retryDelay = captureRetry
		active     bool     // the daemon currently wants audio
		wanted     []string // who is recording, for the status report
		device     = override
		startedAt  time.Time
	)
	stop := func() {
		if capture != nil {
			capture.Stop()
			capture, captureCh = nil, nil
		}
		retry = nil
		// Audio captured for a recording that has ended must not trail into
		// the next one — and neither must its "fell back" notice, which can be
		// pushed while Stop above is still being awaited.
		frames.drain()
		select {
		case <-fellBack:
		default:
		}
	}
	defer stop()
	scheduleRetry := func() {
		retry = time.After(retryDelay)
		retryDelay = min(retryDelay*2, captureRetryMax)
	}
	start := func() {
		started, err := startCapture(device, func(pcm []byte) {
			frames.push(slices.Clone(pcm)) // drops when the network is behind
		}, func() {
			select {
			case fellBack <- struct{}{}:
			default:
			}
		})
		if err != nil {
			report(Status{State: StateError, Detail: Describe(err)})
			if !IsNoTool(err) {
				scheduleRetry()
			}
			return
		}
		capture, captureCh = started, started.Done()
		startedAt = time.Now()
		report(Status{State: StateLive, Instances: wanted, FellBack: started.FellBack()})
	}

	for {
		select {
		case <-ctx.Done():
			// Without this the loop can park forever: the recv goroutine leaves
			// SILENTLY on cancellation (it may be blocked handing over a demand,
			// so it cannot always report), after which no other case can fire —
			// Run would never return, stop() would never run, and the recorder
			// (whose context is its own) would hold the microphone open for the
			// life of the process.
			return attached, ctx.Err()
		case err := <-recvErr:
			return attached, err
		case demand := <-demands:
			// The daemon greets every accepted stream with its current demand,
			// so the first frame doubles as "attached".
			attached = true
			active = demand.GetActive()
			if override == "" {
				device = demand.GetDevice() // applies to the NEXT capture start
			}
			if !active {
				wanted = nil
				retryDelay = captureRetry
				stop()
				report(Status{State: StateIdle})
				continue
			}
			wanted = demand.GetInstances()
			// retry == nil: a recorder that is failing is already on a back-off
			// timer; a demand change (another instance joining) must not
			// respawn it past that.
			if capture == nil && retry == nil {
				start()
			} else if capture == nil {
				// waiting out the back-off; the retry will pick `wanted` up
			} else {
				report(Status{State: StateLive, Instances: wanted, FellBack: capture.FellBack()})
			}
		case <-captureCh:
			// The recorder died on its own while demand was active.
			detail := "recorder exited"
			if err := capture.Err(); err != nil {
				detail = err.Error()
			}
			capture, captureCh = nil, nil
			if time.Since(startedAt) >= captureHealthy {
				retryDelay = captureRetry // it had been working: start over
			}
			report(Status{State: StateError, Detail: detail})
			scheduleRetry()
		case <-fellBack:
			// The configured device would not open; the default is live instead.
			// Ask the capture rather than assume: it is the one that knows.
			if capture != nil {
				report(Status{State: StateLive, Instances: wanted, FellBack: capture.FellBack()})
			}
		case <-retry:
			retry = nil
			if active && capture == nil {
				start()
			}
		}
	}
}
