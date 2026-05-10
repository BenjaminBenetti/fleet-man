package agentdetect

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// allFor wraps a sessionName→content map into a backend.AllSessions
// with OK=true on every capture.
func allFor(sessions map[string]string) backend.AllSessions {
	out := backend.AllSessions{Sessions: make(map[string]backend.ScreenCapture, len(sessions)), OK: true}
	for name, content := range sessions {
		out.Sessions[name] = backend.ScreenCapture{Content: content, OK: true}
	}
	return out
}

func TestCountDiffs(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "hello", "hello", 0},
		{"one diff", "hello", "hallo", 1},
		{"all diff", "abc", "xyz", 3},
		{"different lengths", "ab", "abcde", 3},
		{"empty vs content", "", "hello", 5},
		{"both empty", "", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countDiffs(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("countDiffs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestTmuxPaneChangeDetector(t *testing.T) {
	now := time.Now()

	t.Run("no sessions means not running", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		got := d.Detect(backend.AllSessions{Sessions: map[string]backend.ScreenCapture{}, OK: true}, now)
		if got != StateNotRunning {
			t.Fatalf("got %d, want StateNotRunning", got)
		}
	})

	t.Run("first capture assumes waiting", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		got := d.Detect(allFor(map[string]string{"main": "spinner content here"}), now)
		if got != StateWaiting {
			t.Fatalf("got %d, want StateWaiting on first capture", got)
		}
	})

	t.Run("screen changed above threshold marks working", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		d.Detect(allFor(map[string]string{"main": "hello world"}), now.Add(-1*time.Second))
		got := d.Detect(allFor(map[string]string{"main": "XXXXX world"}), now)
		if got != StateWorking {
			t.Fatalf("got %d, want StateWorking", got)
		}
	})

	t.Run("screen changed below threshold stays waiting", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		d.Detect(allFor(map[string]string{"main": "hello world"}), now.Add(-31*time.Second))
		got := d.Detect(allFor(map[string]string{"main": "hellX world"}), now) // 1 char diff < threshold
		if got != StateWaiting {
			t.Fatalf("got %d, want StateWaiting", got)
		}
	})

	t.Run("no change but recent activity stays working", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		// Cycle 1: seed prev content.
		d.Detect(allFor(map[string]string{"main": "hello world"}), now.Add(-10*time.Second))
		// Cycle 2: big change records lastChange.
		d.Detect(allFor(map[string]string{"main": "XXXXX world"}), now.Add(-9*time.Second))
		// Cycle 3: identical content, but well within activity window.
		got := d.Detect(allFor(map[string]string{"main": "XXXXX world"}), now)
		if got != StateWorking {
			t.Fatalf("got %d, want StateWorking", got)
		}
	})

	t.Run("no change and stale activity means waiting", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		d.Detect(allFor(map[string]string{"main": "hello world"}), now.Add(-31*time.Second))
		got := d.Detect(allFor(map[string]string{"main": "hello world"}), now)
		if got != StateWaiting {
			t.Fatalf("got %d, want StateWaiting", got)
		}
	})

	t.Run("activity in any session marks container working", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		d.Detect(allFor(map[string]string{
			"main":      "static content",
			"session-2": "old agent content",
		}), now.Add(-1*time.Second))
		// session-2 has a big change (agent active there); main is unchanged.
		got := d.Detect(allFor(map[string]string{
			"main":      "static content",
			"session-2": "NEW agent content",
		}), now)
		if got != StateWorking {
			t.Fatalf("got %d, want StateWorking (session-2 changed)", got)
		}
	})

	t.Run("idle in all sessions means waiting", func(t *testing.T) {
		d := NewTmuxPaneChangeDetector()
		d.Detect(allFor(map[string]string{
			"main":      "static content",
			"session-2": "another static",
		}), now.Add(-30*time.Second))
		got := d.Detect(allFor(map[string]string{
			"main":      "static content",
			"session-2": "another static",
		}), now)
		if got != StateWaiting {
			t.Fatalf("got %d, want StateWaiting", got)
		}
	})
}
