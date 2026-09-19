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
	// fellBack: a device was asked for but the system default is being
	// recorded, because this machine does not have that device.
	fellBack bool

	mu  sync.Mutex
	err error
}

// Start launches the recorder for deviceID ("" = system default) and delivers
// raw PCM to sink from a single goroutine until Stop is called or the recorder
// exits. The slice passed to sink is only valid for the duration of the call.
//
// A configured device that cannot be opened (unplugged, or an id from another
// machine) falls back to the system default once, rather than leaving the user
// with a dead microphone.
func Start(deviceID string, sink func([]byte)) (*Capture, error) {
	ctx, cancel := context.WithCancel(context.Background())
	_, used, err := Command(ctx, deviceID)
	if err != nil {
		cancel()
		return nil, err
	}
	capture := &Capture{
		cancel:   cancel,
		done:     make(chan struct{}),
		fellBack: deviceID != "" && used == "" && os.Getenv(EnvCapture) == "",
	}
	go func() {
		defer close(capture.done)
		defer cancel()
		produced, err := record(ctx, deviceID, sink)
		if err != nil && !produced && deviceID != "" && ctx.Err() == nil {
			_, err = record(ctx, "", sink)
		}
		if ctx.Err() != nil {
			// Stopped on purpose; the recorder's "killed" exit is not an error.
			err = nil
		}
		capture.mu.Lock()
		capture.err = err
		capture.mu.Unlock()
	}()
	return capture, nil
}

// Stop ends the capture and waits for the recorder to be reaped, so the real
// microphone is released by the time Stop returns.
func (c *Capture) Stop() {
	c.cancel()
	<-c.done
}

// FellBack reports that the configured device is not available on this machine
// and the system default is being recorded instead.
func (c *Capture) FellBack() bool { return c.fellBack }

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
func record(ctx context.Context, deviceID string, sink func([]byte)) (produced bool, err error) {
	cmd, _, err := Command(ctx, deviceID)
	if err != nil {
		return false, err
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
		return false, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	buf := make([]byte, ChunkBytes)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			produced = true
			sink(buf[:n])
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
		return produced, fmt.Errorf("%s: %w: %s", cmd.Path, waitErr, detail)
	}
	return produced, fmt.Errorf("%s: %w", cmd.Path, waitErr)
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
