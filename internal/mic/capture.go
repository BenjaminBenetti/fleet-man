package mic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// stderrTailBytes caps how much recorder stderr is kept for an error message.
const stderrTailBytes = 512

// Capture is one running recorder. It owns the recorder process; Stop (or the
// process dying) ends it, after which Done is closed and Err says why.
type Capture struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
	// fellBack: a device was asked for but the system default is being recorded
	// — because this machine does not list that device (known at Start), or
	// because it could not be opened (known only once the recorder has failed).
	fellBack bool
	// onFallback, if set, is called when the second case happens mid-capture.
	onFallback func()
}

// Start launches the recorder for deviceID ("" = system default) and delivers
// raw PCM to sink from a single goroutine until Stop is called or the recorder
// exits. The slice passed to sink is only valid for the duration of the call.
//
// A configured device that cannot be opened (unplugged, or an id from another
// machine) falls back to the system default once, rather than leaving the user
// with a dead microphone.
func Start(deviceID string, sink func([]byte)) (*Capture, error) {
	return StartNotify(deviceID, sink, nil)
}

// StartNotify is Start, plus a callback for the one thing Start cannot know up
// front: that the configured device turned out not to open and the system
// default is being recorded instead. "Which microphone is actually open" is the
// one thing a status read-out must not get wrong.
func StartNotify(deviceID string, sink func([]byte), onFallback func()) (*Capture, error) {
	// Resolved ONCE and handed to record: resolving validates the device, which
	// can mean enumerating — not something to pay twice per capture start.
	argv, used, err := recorderArgv(deviceID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	capture := &Capture{
		cancel:     cancel,
		done:       make(chan struct{}),
		fellBack:   deviceID != "" && used == "" && os.Getenv(EnvCapture) == "",
		onFallback: onFallback,
	}
	return capture.run(ctx, argv, deviceID, used, sink), nil
}

func (c *Capture) run(ctx context.Context, argv []string, deviceID, used string, sink func([]byte)) *Capture {
	capture, cancel := c, c.cancel
	go func() {
		defer close(capture.done)
		defer cancel()
		produced, err := record(ctx, argv, deviceID, sink)
		if err != nil && !produced && used != "" && ctx.Err() == nil {
			// The device was enumerated but cannot be opened (unplugged since):
			// one try on the system default.
			if fallback, _, argvErr := recorderArgv(""); argvErr == nil {
				capture.mu.Lock()
				capture.fellBack = true
				notify := capture.onFallback
				capture.mu.Unlock()
				if notify != nil {
					notify()
				}
				_, err = record(ctx, fallback, "", sink)
			}
		}
		if ctx.Err() != nil {
			// Stopped on purpose; the recorder's "killed" exit is not an error.
			err = nil
		}
		capture.mu.Lock()
		capture.err = err
		capture.mu.Unlock()
	}()
	return capture
}

// Stop ends the capture and waits for the recorder to be reaped, so the real
// microphone is released by the time Stop returns.
func (c *Capture) Stop() {
	c.cancel()
	<-c.done
}

// FellBack reports that the configured device is not available on this machine
// and the system default is being recorded instead.
func (c *Capture) FellBack() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fellBack
}

// Done is closed once the recorder has exited, for whatever reason.
func (c *Capture) Done() <-chan struct{} { return c.done }

// Err reports why the recorder exited; nil after a deliberate Stop. Only
// meaningful once Done is closed.
func (c *Capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// record runs one recorder process to completion. produced reports whether it
// ever delivered audio — the signal that separates "this device cannot be
// opened" (worth retrying on the default) from a recorder that died mid-stream.
func record(ctx context.Context, argv []string, deviceID string, sink func([]byte)) (produced bool, err error) {
	// guardedCommand: own process group, killed as a group on cancel, and tied
	// to fleet's own lifetime by a lifeline — see there for why all three.
	cmd, release, err := guardedCommand(ctx, argv...)
	if err != nil {
		return false, err
	}
	defer release()
	cmd.Env = append(os.Environ(), EnvDevice+"="+deviceID)
	// Every recorder runs under the sh wrapper, so cmd.Path is /bin/sh for all
	// of them; errors are user-visible (the Status row) and must name the
	// recorder that actually failed.
	recorder := argv[0]
	if os.Getenv(EnvCapture) != "" {
		recorder = EnvCapture + " recorder"
	}
	// A recorder that ignores the kill (or leaves a grandchild holding the pipe)
	// must not wedge Stop.
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start %s: %w", recorder, err)
	}

	// Frames leave here as WHOLE samples. A pipe read can return an odd number
	// of bytes, and downstream drops whole frames (the send queue here, the
	// daemon's sink queues): one dropped odd-length frame would shift the sample
	// phase of everything after it, turning speech into byte-swapped noise. So
	// the odd byte is held back and leads the next frame.
	buf := make([]byte, ChunkBytes+1)
	held := 0 // 0 or 1 bytes carried at buf[0]
	for {
		n, readErr := stdout.Read(buf[held:])
		if n > 0 {
			produced = true
			total := held + n
			whole := total &^ 1
			if whole > 0 {
				sink(buf[:whole])
			}
			held = total - whole
			if held == 1 {
				buf[0] = buf[whole]
			}
		}
		if readErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		// A recorder has no business exiting on its own; treat a clean exit as
		// the stream ending so the caller can restart it.
		return produced, io.EOF
	}
	if detail := stderr.String(); detail != "" {
		return produced, fmt.Errorf("%s: %w: %s", recorder, waitErr, detail)
	}
	return produced, fmt.Errorf("%s: %w", recorder, waitErr)
}

// tailBuffer keeps the last stderrTailBytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if extra := t.buf.Len() - stderrTailBytes; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}

// IsNoTool reports whether err means this machine cannot capture at all — no
// recorder, or only fleet's own virtual microphone — as opposed to a recorder
// that failed. Retrying such an error is pointless.
func IsNoTool(err error) bool {
	return errors.Is(err, ErrNoCaptureTool) || errors.Is(err, ErrVirtualMic)
}
