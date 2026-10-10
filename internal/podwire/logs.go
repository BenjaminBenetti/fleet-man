package podwire

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The agent's own warnings and errors (socket relays, mic and output pipes,
// status-mod writes, staging) reach fleet.log through LogLimiter, at the level
// the agent gave them, so the daemon log viewer's Error and Warn filters show
// them as they show the same failures on Docker, where those pieces run in
// fleetd.
const (
	// LogLimit is how many records a pod writes to fleet.log per LogWindow;
	// past it they are counted and one "suppressed N" line follows.
	LogLimit  = 20
	LogWindow = time.Minute

	maxLogMsg   = 1024
	maxLogAttrs = 16
	maxLogValue = 512
)

// validLogKey reports whether k is a record attribute key fleetd writes: a
// letter, then up to 31 letters, digits, '_', '.' or '-'.
func validLogKey(k string) bool {
	if len(k) == 0 || len(k) > 32 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !letter {
			return false
		}
		if !letter && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// reservedLogKeys are attribute keys the record's logger owns (the pod's
// fleet and instance tag, the line's own fields): an agent's record never
// overrides them.
var reservedLogKeys = []string{"fleet", "instance", "pid", "time", "level", "msg", "source"}

// LogLimiter writes the LOG records one pod's agent sends to logger: at the
// record's level, with "pod-agent: " before its message, at most limit records
// per window, then one warning saying how many were suppressed. logger is
// expected to carry the pod's fleet and instance (logger.With). It is safe for
// concurrent use.
type LogLimiter struct {
	logger *slog.Logger
	limit  int
	window time.Duration

	mu         sync.Mutex
	start      time.Time
	count      int
	suppressed int
	timer      *time.Timer
	closed     bool
}

// NewLogLimiter writes to logger, at most limit records per window (LogLimit
// per LogWindow when either is 0).
func NewLogLimiter(logger *slog.Logger, limit int, window time.Duration) *LogLimiter {
	if limit <= 0 {
		limit = LogLimit
	}
	if window <= 0 {
		window = LogWindow
	}
	return &LogLimiter{logger: logger, limit: limit, window: window}
}

// Log writes rec, or counts it when the window's limit is reached.
func (l *LogLimiter) Log(rec LogRecord) {
	if l == nil || l.logger == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	now := time.Now()
	if l.start.IsZero() || now.Sub(l.start) >= l.window {
		l.flushLocked()
		l.start, l.count = now, 0
	}
	if l.count >= l.limit {
		l.suppressed++
		if l.timer == nil {
			l.timer = time.AfterFunc(l.window-now.Sub(l.start), l.flush)
		}
		l.mu.Unlock()
		return
	}
	l.count++
	l.mu.Unlock()
	l.logger.Log(context.Background(), logLevel(rec.Level), "pod-agent: "+clip(oneLine(rec.Msg), maxLogMsg), logArgs(rec.Attrs)...)
}

// Close writes the pending "suppressed" line, if any; later records are
// dropped.
func (l *LogLimiter) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.flushLocked()
	l.closed = true
	l.mu.Unlock()
}

func (l *LogLimiter) flush() {
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
}

// flushLocked writes the window's "suppressed" line and ends the window.
func (l *LogLimiter) flushLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.suppressed > 0 && !l.closed {
		l.logger.Warn("pod-agent: suppressed log lines", "count", l.suppressed, "window", l.window.String())
	}
	l.suppressed = 0
	l.start, l.count = time.Time{}, 0
}

// logLevel maps a record's level onto slog's; anything unknown is info.
func logLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "error":
		return slog.LevelError
	case "warn", "warning":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// logArgs turns a record's attributes into slog args, sorted by key, keeping
// only well-formed keys the logger does not own and clipping values.
func logArgs(attrs map[string]string) []any {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		if validLogKey(k) && !slices.Contains(reservedLogKeys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) > maxLogAttrs {
		keys = keys[:maxLogAttrs]
	}
	args := make([]any, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, k, clip(oneLine(attrs[k]), maxLogValue))
	}
	return args
}

// oneLine replaces control characters, so one record is one line whatever the
// agent sent.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// clip cuts s to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
