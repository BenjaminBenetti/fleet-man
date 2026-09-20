package micsink

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// demandPollInterval is the safety net under `pactl subscribe`: a missed or
	// coalesced event still converges within this long.
	demandPollInterval = 2 * time.Second
	// maxBlindRecounts is how many CONSECUTIVE failed recounts are tolerated
	// before demand is forced off. One failure is a blip and means "unchanged"
	// (closing the mic mid-sentence is its own bug); but this is the privacy
	// gate, and it must fail CLOSED: a server that keeps the subscription alive
	// while never answering must not hold the human's microphone open.
	maxBlindRecounts = 3
)

// pactlTimeout bounds every pactl / pulseaudio invocation in this package. A
// wedged sound server must cost a bounded wait — never a stuck watcher (the
// microphone's gate), a sink that cannot shut down, or a daemon-side slot that
// waits forever for a "ready" that is not coming. A var so tests can shorten it.
var pactlTimeout = 3 * time.Second

// Run is `fleet mic sink`: bring the virtual microphone up, then feed it from
// stdin for as long as stdin lasts, reporting demand on stdout. It returns when
// stdin reaches EOF (the daemon detached — the normal exit), when ctx is
// cancelled, or when the sound server is lost and cannot be restarted.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	var emitMu sync.Mutex
	emit := func(format string, args ...any) {
		emitMu.Lock()
		defer emitMu.Unlock()
		fmt.Fprintf(stdout, format+"\n", args...)
	}

	if err := Ensure(); err != nil {
		emit(EventError+" %s", oneLine(err.Error()))
		return err
	}
	pipe, err := openFIFO(path("pcm"))
	if err != nil {
		emit(EventError+" open fifo: %s", oneLine(err.Error()))
		return fmt.Errorf("open %s: %w", path("pcm"), err)
	}
	defer pipe.close()
	// Starts closed, and closing drains: whatever a previous sink left in the
	// pipe is gone before the first recorder can hear it.
	pipe.setOpen(false)

	ctx, cancel := context.WithCancel(ctx)
	// On the way out: stop the watcher and WAIT for it, before the deferred
	// pipe.close above runs (defers are LIFO). The watcher opens/closes/drains
	// the FIFO; letting it outlive Run would have it act on a closed — or, worse,
	// reused — file descriptor.
	watchDone := make(chan struct{})
	defer func() {
		cancel()
		<-watchDone
	}()

	// The watcher opens and closes the microphone as recorders come and go. The
	// gate itself lives inside the fifo (see fifo.open) so that closing — which
	// drains — cannot interleave with a write that already passed the check.
	var demand atomic.Bool
	watchErr := make(chan error, 1)
	go func() {
		defer close(watchDone)
		watchErr <- watchDemand(ctx, func(active bool) {
			if demand.Swap(active) == active {
				return
			}
			pipe.setOpen(active)
			if active {
				emit(EventDemand + " " + DemandOn)
			} else {
				emit(EventDemand + " " + DemandOff)
			}
		})
	}()

	pumpDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				pipe.write(buf[:n]) // a no-op while nobody is recording
			}
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				pumpDone <- err
				return
			}
		}
	}()

	emit(EventReady)
	select {
	case err := <-pumpDone:
		return err
	case err := <-watchErr:
		if ctx.Err() != nil {
			return nil
		}
		emit(EventError+" %s", oneLine(err.Error()))
		return err
	case <-ctx.Done():
		return nil
	}
}

// watchDemand reports, through set, whether any recorder is attached to the
// virtual microphone. It follows `pactl subscribe` for immediacy (the client
// only opens the real mic once demand is reported, so every millisecond here is
// clipped off the start of the user's sentence) and re-counts on a slow ticker
// as a safety net. It returns when the subscription dies — the server is gone.
func watchDemand(ctx context.Context, set func(bool)) error {
	cmd := exec.CommandContext(ctx, "pactl", "subscribe")
	cmd.Env = pulseEnv()
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pactl subscribe: %w", err)
	}
	// Reaped on every way out; without this each Run would leave a zombie behind.
	// KILLED first: not every way out cancels ctx (the subscription ending is
	// one), and a Wait on a pactl that is still alive would park this watcher —
	// freezing demand at its last value, possibly ON.
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	events := make(chan struct{}, 1)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "source-output") {
				select {
				case events <- struct{}{}:
				default: // a recount is already pending
				}
			}
		}
	}()

	// A probe that ERRORS says nothing, and is treated as "unchanged" — never as
	// "nobody is recording": that would close the microphone mid-sentence, drain
	// audio a still-attached recorder was about to read, and take up to a poll
	// interval to recover. A server that is really gone ends the subscription.
	blind := 0
	recount := func() {
		attached, ok := recorderAttached(ctx)
		switch {
		case ok:
			blind = 0
			set(attached)
		case ctx.Err() != nil:
			// shutting down, not a failed probe
		default:
			if blind++; blind >= maxBlindRecounts {
				set(false)
			}
		}
	}
	recount()
	ticker := time.NewTicker(demandPollInterval)
	defer ticker.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return fmt.Errorf("lost the pulseaudio server (see %s)", path("pulse.log"))
			}
			recount()
		case <-ticker.C:
			recount()
		case <-ctx.Done():
			return nil
		}
	}
}

// recorderAttached reports whether at least one source-output is recording from
// the fleet microphone (rather than, say, the null sink's monitor). ok is false
// when the server could not be asked.
func recorderAttached(ctx context.Context) (attached, ok bool) {
	sources, err := pactl(ctx, "list", "short", "sources")
	if err != nil {
		return false, false
	}
	outputs, err := pactl(ctx, "list", "short", "source-outputs")
	if err != nil {
		return false, false
	}
	return countRecorders(sources, outputs) > 0, true
}

// countRecorders joins `pactl list short sources` (index, name, …) with
// `pactl list short source-outputs` (index, source index, …) and counts the
// outputs attached to the fleet microphone.
func countRecorders(sources, outputs string) int {
	micIndex := ""
	for line := range strings.SplitSeq(sources, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == SourceName {
			micIndex = fields[0]
		}
	}
	if micIndex == "" {
		return 0
	}
	count := 0
	for line := range strings.SplitSeq(outputs, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == micIndex {
			count++
		}
	}
	return count
}

// pactl runs one bounded pactl query against the private server.
func pactl(ctx context.Context, args ...string) (string, error) {
	cmd, cancel := boundedCommand(ctx, "pactl", args...)
	defer cancel()
	out, err := cmd.Output()
	return string(out), err
}

// boundedCommand builds a pulse command that cannot outlive pactlTimeout (or
// ctx), with WaitDelay so a child holding the pipes cannot wedge Wait either.
func boundedCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, pactlTimeout)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = pulseEnv()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

// oneLine keeps an error on a single protocol line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
