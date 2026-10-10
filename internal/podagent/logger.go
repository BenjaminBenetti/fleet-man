package podagent

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// logQueue is how many records the Logger keeps while fleetd cannot take
// them; past it they are counted and dropped.
const logQueue = 64

// Logger is how the agent reports its own warnings and errors: as LOG
// records on the control stream, which fleetd writes to fleet.log at the same
// level, tagged with the pod's fleet and instance. Before the control stream
// is up, records wait (up to logQueue); without one (it is gone, or there
// never is one) they go to stderr. It is safe for concurrent use.
type Logger struct {
	mu      sync.Mutex
	w       *podwire.FrameWriter
	queue   []podwire.LogRecord
	dropped int
	// detached: no control stream is coming, so records go to stderr.
	detached bool
	stderr   io.Writer
}

// newLogger keeps records until attach gives it the control stream.
func newLogger(stderr io.Writer) *Logger { return &Logger{stderr: stderr} }

// newStderrLogger writes every record to stderr: for a direct exec, which
// has no control stream.
func newStderrLogger(stderr io.Writer) *Logger { return &Logger{stderr: stderr, detached: true} }

// Error, Warn and Info log msg with key/value pairs.
func (l *Logger) Error(msg string, kv ...string) { l.log("error", msg, kv) }
func (l *Logger) Warn(msg string, kv ...string)  { l.log("warn", msg, kv) }
func (l *Logger) Info(msg string, kv ...string)  { l.log("info", msg, kv) }

func (l *Logger) log(level, msg string, kv []string) {
	rec := podwire.LogRecord{Level: level, Msg: msg}
	if len(kv) > 0 {
		rec.Attrs = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			rec.Attrs[kv[i]] = kv[i+1]
		}
	}
	l.mu.Lock()
	w := l.w
	if w == nil && l.detached {
		l.mu.Unlock()
		l.toStderr(rec)
		return
	}
	if w == nil {
		if len(l.queue) < logQueue {
			l.queue = append(l.queue, rec)
		} else {
			l.dropped++
		}
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	if err := w.WriteJSON(podwire.FrameData, podwire.ControlMessage{Type: podwire.MsgLog, Log: &rec}); err != nil {
		l.toStderr(rec)
	}
}

// attach starts sending records on a control stream, queued ones first.
func (l *Logger) attach(w *podwire.FrameWriter) {
	l.mu.Lock()
	queue, dropped := l.queue, l.dropped
	l.queue, l.dropped = nil, 0
	l.w = w
	l.mu.Unlock()
	for i := range queue {
		_ = w.WriteJSON(podwire.FrameData, podwire.ControlMessage{Type: podwire.MsgLog, Log: &queue[i]})
	}
	if dropped > 0 {
		l.Warn("dropped log records while fleetd was away", "count", fmt.Sprint(dropped))
	}
}

// detach stops sending: the control stream is gone (or never came). Queued
// records, and every later one, go to stderr.
func (l *Logger) detach() {
	l.mu.Lock()
	l.w = nil
	l.detached = true
	queue := l.queue
	l.queue = nil
	l.mu.Unlock()
	for _, rec := range queue {
		l.toStderr(rec)
	}
}

func (l *Logger) toStderr(rec podwire.LogRecord) {
	if l.stderr == nil {
		return
	}
	keys := make([]string, 0, len(rec.Attrs))
	for k := range rec.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "pod-agent: %s: %s", rec.Level, rec.Msg)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%q", k, rec.Attrs[k])
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(l.stderr, b.String())
}
