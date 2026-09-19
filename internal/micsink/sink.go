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

// demandPollInterval is the safety net under `pactl subscribe`: a missed or
// coalesced event still converges within this long.
const demandPollInterval = 2 * time.Second

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
		emit("error %s", oneLine(err.Error()))
		return err
	}
	pipe, err := openFIFO(path("pcm"))
	if err != nil {
		emit("error open fifo: %s", oneLine(err.Error()))
		return fmt.Errorf("open %s: %w", path("pcm"), err)
	}
	defer pipe.close()
	pipe.drain()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// demand gates the pump. The watcher flips it; on the falling edge it also
	// drains the FIFO (see fifo.drain).
	var demand atomic.Bool
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- watchDemand(ctx, func(active bool) {
			if demand.Swap(active) == active {
				return
			}
			if !active {
				pipe.drain()
			}
			if active {
				emit("demand 1")
			} else {
				emit("demand 0")
			}
		})
	}()

	pumpDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 && demand.Load() {
				pipe.write(buf[:n])
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

	emit("ready")
	select {
	case err := <-pumpDone:
		return err
	case err := <-watchErr:
		if ctx.Err() != nil {
			return nil
		}
		emit("error %s", oneLine(err.Error()))
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

	set(recorderAttached())
	ticker := time.NewTicker(demandPollInterval)
	defer ticker.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				_ = cmd.Wait()
				return fmt.Errorf("lost the pulseaudio server (see %s)", path("pulse.log"))
			}
			set(recorderAttached())
		case <-ticker.C:
			set(recorderAttached())
		case <-ctx.Done():
			return nil
		}
	}
}

// recorderAttached reports whether at least one source-output is recording from
// the fleet microphone (rather than, say, the null sink's monitor).
func recorderAttached() bool {
	sources, err := pactl("list", "short", "sources")
	if err != nil {
		return false
	}
	outputs, err := pactl("list", "short", "source-outputs")
	if err != nil {
		return false
	}
	return countRecorders(sources, outputs) > 0
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

func pactl(args ...string) (string, error) {
	cmd := exec.Command("pactl", args...)
	cmd.Env = pulseEnv()
	out, err := cmd.Output()
	return string(out), err
}

// oneLine keeps an error on a single protocol line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
