package podwire

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// capture is a slog handler that keeps records.
type capture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

func (c *capture) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Record(nil), c.records...)
}

func attrsOf(r slog.Record) map[string]string {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

func TestLogLimiterLevelsAndAttrs(t *testing.T) {
	c := &capture{}
	l := NewLogLimiter(slog.New(c), 0, 0)
	l.Log(LogRecord{Level: "error", Msg: "relay failed", Attrs: map[string]string{"sock": "ssh-agent", "fleet": "spoofed", "bad key!": "x"}})
	l.Log(LogRecord{Level: "warn", Msg: "two\nlines"})
	l.Log(LogRecord{Level: "whatever", Msg: strings.Repeat("é", 2000)})
	recs := c.snapshot()
	if len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
	if recs[0].Level != slog.LevelError || recs[0].Message != "pod-agent: relay failed" {
		t.Fatalf("record 0: %v %q", recs[0].Level, recs[0].Message)
	}
	attrs := attrsOf(recs[0])
	if attrs["sock"] != "ssh-agent" || attrs["fleet"] != "" || attrs["bad key!"] != "" {
		t.Fatalf("attrs %v: the agent may not set the logger's own keys or malformed ones", attrs)
	}
	if recs[1].Level != slog.LevelWarn || strings.Contains(recs[1].Message, "\n") {
		t.Fatalf("record 1: %v %q", recs[1].Level, recs[1].Message)
	}
	if recs[2].Level != slog.LevelInfo || len(recs[2].Message) > maxLogMsg+len("pod-agent: …") || !strings.HasSuffix(recs[2].Message, "é…") {
		t.Fatalf("record 2 is not clipped on a rune boundary: %d bytes", len(recs[2].Message))
	}
}

func TestLogLimiterSuppresses(t *testing.T) {
	c := &capture{}
	l := NewLogLimiter(slog.New(c), 3, 200*time.Millisecond)
	for i := range 10 {
		l.Log(LogRecord{Level: "warn", Msg: "flood", Attrs: map[string]string{"i": string(rune('0' + i))}})
	}
	if n := len(c.snapshot()); n != 3 {
		t.Fatalf("%d records written within the window, want 3", n)
	}
	// The window's end writes one "suppressed" line, on its own.
	deadline := time.Now().Add(2 * time.Second)
	for len(c.snapshot()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	recs := c.snapshot()
	if len(recs) != 4 {
		t.Fatalf("%d records after the window", len(recs))
	}
	last := recs[3]
	if last.Level != slog.LevelWarn || !strings.Contains(last.Message, "suppressed") || attrsOf(last)["count"] != "7" {
		t.Fatalf("suppressed line: %v %q %v", last.Level, last.Message, attrsOf(last))
	}
	// A new window takes records again.
	l.Log(LogRecord{Level: "info", Msg: "later"})
	if n := len(c.snapshot()); n != 5 {
		t.Fatalf("%d records after a new window", n)
	}
	l.Close()
	l.Log(LogRecord{Level: "info", Msg: "after close"})
	if n := len(c.snapshot()); n != 5 {
		t.Fatal("a closed limiter wrote")
	}
}

// TestLogRecordsReachFleetLog writes agent records through the limiter into
// fleet.log the way fleetd wires it (flog's logger, tagged with the pod's
// fleet and instance) and checks the lines the daemon log viewer filters on.
func TestLogRecordsReachFleetLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	l := NewLogLimiter(flog.L().With("fleet", "web", "instance", "web-1"), 2, time.Minute)
	l.Log(LogRecord{Level: "error", Msg: "ssh-agent relay failed", Attrs: map[string]string{"err": "connection refused"}})
	l.Log(LogRecord{Level: "warn", Msg: "mic pipe stalled"})
	l.Log(LogRecord{Level: "error", Msg: "dropped"})
	l.Close()
	path := filepath.Join(home, ".fleet", "fleet.log")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("flog's file was opened before this test set HOME (-count > 1)")
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("fleet.log:\n%s", data)
	}
	for i, want := range []string{
		`level=ERROR msg="pod-agent: ssh-agent relay failed" pid=`,
		`level=WARN msg="pod-agent: mic pipe stalled" pid=`,
		`level=WARN msg="pod-agent: suppressed log lines" pid=`,
	} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "fleet=web instance=web-1") {
			t.Errorf("line %d %q, want %q tagged fleet=web instance=web-1", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], `err="connection refused"`) || !strings.Contains(lines[2], "count=1") {
		t.Errorf("attrs: %q / %q", lines[0], lines[2])
	}
	if bytes.Contains(data, []byte("dropped")) {
		t.Error("a record over the limit was written")
	}
}
