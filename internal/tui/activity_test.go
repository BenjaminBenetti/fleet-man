package tui

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// allFor is a test helper that wraps a single sessionName→content
// mapping into a backend.AllSessions with OK=true.
func allFor(sessions map[string]string) backend.AllSessions {
	out := backend.AllSessions{Sessions: make(map[string]backend.ScreenCapture, len(sessions)), OK: true}
	for name, content := range sessions {
		out.Sessions[name] = backend.ScreenCapture{Content: content, OK: true}
	}
	return out
}

func TestActivityTrackerUpdate(t *testing.T) {
	now := time.Now()

	t.Run("first capture assumes waiting", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "spinner content here"}),
		}, nil, []string{"c1"}, now)
		if tr.State("c1") != agentdetect.StateWaiting {
			t.Fatalf("got %d, want StateWaiting on first capture", tr.State("c1"))
		}
	})

	t.Run("screen changed above threshold marks working across cycles", func(t *testing.T) {
		tr := NewActivityTracker()
		// Seed the per-container detector with prior content.
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "hello world"}),
		}, nil, []string{"c1"}, now.Add(-1*time.Second))
		// Big change — should flip to working.
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "XXXXX world"}),
		}, nil, []string{"c1"}, now)
		if tr.State("c1") != agentdetect.StateWorking {
			t.Fatalf("got %d, want StateWorking", tr.State("c1"))
		}
	})

	t.Run("exec failure preserves previous state", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.states["c1"] = agentdetect.StateWorking
		tr.tools["c1"] = state.AgentToolClaude
		tr.detectors["c1"] = agentdetect.NewTmuxPaneChangeDetector()
		tr.Update(map[string]backend.AllSessions{
			"c1": {OK: false},
		}, nil, []string{"c1"}, now)
		if tr.State("c1") != agentdetect.StateWorking {
			t.Fatalf("got %d, want StateWorking (preserved)", tr.State("c1"))
		}
		if tr.Tool("c1") != state.AgentToolClaude {
			t.Fatalf("tool not preserved: got %q", tr.Tool("c1"))
		}
	})

	t.Run("exec failure on fresh tracker means not running", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": {OK: false},
		}, nil, []string{"c1"}, now)
		if tr.State("c1") != agentdetect.StateNotRunning {
			t.Fatalf("got %d, want StateNotRunning", tr.State("c1"))
		}
	})

	t.Run("ok but no sessions means not running", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": {Sessions: map[string]backend.ScreenCapture{}, OK: true},
		}, nil, []string{"c1"}, now)
		if tr.State("c1") != agentdetect.StateNotRunning {
			t.Fatalf("got %d, want StateNotRunning", tr.State("c1"))
		}
	})

	t.Run("detects tool from probe results", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "some screen content"}),
		}, map[string]string{"c1": "codex"}, []string{"c1"}, now)
		if tr.Tool("c1") != state.AgentToolCodex {
			t.Fatalf("got %q, want %q", tr.Tool("c1"), state.AgentToolCodex)
		}
	})

	t.Run("tool detected even when no sessions captured", func(t *testing.T) {
		// Regression: tool detection runs independently of session
		// capture so the UI shows the right label even when tmux has
		// not started serving sessions yet.
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": {Sessions: map[string]backend.ScreenCapture{}, OK: true},
		}, map[string]string{"c1": "claude"}, []string{"c1"}, now)
		if tr.Tool("c1") != state.AgentToolClaude {
			t.Fatalf("got %q, want %q", tr.Tool("c1"), state.AgentToolClaude)
		}
	})

	t.Run("clears tool and marks idle when probe finds no agent", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.tools["c1"] = state.AgentToolClaude
		tr.states["c1"] = agentdetect.StateWaiting
		tr.detectors["c1"] = agentdetect.NewTmuxPaneChangeDetector()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "some screen content"}),
		}, map[string]string{"c1": ""}, []string{"c1"}, now)
		if tr.Tool("c1") != "" {
			t.Fatalf("tool should be cleared, got %q", tr.Tool("c1"))
		}
		if tr.State("c1") != agentdetect.StateNotRunning {
			t.Fatalf("got %d, want StateNotRunning", tr.State("c1"))
		}
	})

	t.Run("cleans up removed containers", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.states["c1"] = agentdetect.StateWorking
		tr.states["c2"] = agentdetect.StateWaiting
		tr.tools["c2"] = state.AgentToolClaude
		tr.detectors["c2"] = agentdetect.NewTmuxPaneChangeDetector()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "a"}),
		}, nil, []string{"c1"}, now)
		if tr.State("c2") != agentdetect.StateNotRunning {
			t.Fatal("c2 should have been cleaned up")
		}
		if tr.Tool("c2") != "" {
			t.Fatalf("c2 tool should be cleared, got %q", tr.Tool("c2"))
		}
	})

	t.Run("rebuilds detector when tool changes", func(t *testing.T) {
		// Different tools may want different strategies; when the
		// detected tool changes the tracker must hand out a fresh
		// detector instead of reusing the one tied to the old tool.
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "first"}),
		}, map[string]string{"c1": "claude"}, []string{"c1"}, now)
		first := tr.detectors["c1"]
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "second"}),
		}, map[string]string{"c1": "codex"}, []string{"c1"}, now)
		if tr.detectors["c1"] == first {
			t.Fatal("detector should have been rebuilt when tool changed")
		}
	})

	t.Run("reuses detector when tool stays the same", func(t *testing.T) {
		tr := NewActivityTracker()
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "first"}),
		}, map[string]string{"c1": "claude"}, []string{"c1"}, now)
		first := tr.detectors["c1"]
		tr.Update(map[string]backend.AllSessions{
			"c1": allFor(map[string]string{"main": "second"}),
		}, map[string]string{"c1": "claude"}, []string{"c1"}, now)
		if tr.detectors["c1"] != first {
			t.Fatal("detector should be reused when tool is unchanged")
		}
	})
}
