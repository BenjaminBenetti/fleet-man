package backend

import "testing"

func TestParseToolProbeOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantTool string
		wantOK   bool
	}{
		{"claude detected", "claude\n", "claude", true},
		{"copilot detected", "copilot\n", "copilot", true},
		{"codex detected", "codex\n", "codex", true},
		{"gemini detected", "gemini\n", "gemini", true},
		{"no agent", "-\n", "", true},
		{"empty output (exec failure)", "", "", false},
		{"whitespace only (exec failure)", "  \n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := ParseToolProbeOutput(tt.output)
			if ok != tt.wantOK {
				t.Fatalf("ParseToolProbeOutput(%q) ok = %v, want %v", tt.output, ok, tt.wantOK)
			}
			if tool != tt.wantTool {
				t.Fatalf("ParseToolProbeOutput(%q) tool = %q, want %q", tt.output, tool, tt.wantTool)
			}
		})
	}
}

// ===========================================
// ParseCaptureOutput — sessions
// ===========================================

func TestParseCaptureOutput_Sessions(t *testing.T) {
	mk := func(name, content string) string {
		return sessionMarker + name + "\n" + content
	}

	t.Run("empty input means no sessions", func(t *testing.T) {
		sessions, files, _ := ParseCaptureOutput("")
		if len(sessions) != 0 {
			t.Fatalf("expected 0 sessions, got %d", len(sessions))
		}
		if len(files) != 0 {
			t.Fatalf("expected 0 files, got %d", len(files))
		}
	})

	t.Run("single session with content", func(t *testing.T) {
		input := mk("main", "line1\nline2\n")
		sessions, _, _ := ParseCaptureOutput(input)
		if len(sessions) != 1 {
			t.Fatalf("expected 1 session, got %d", len(sessions))
		}
		capture, ok := sessions["main"]
		if !ok {
			t.Fatalf("session 'main' missing")
		}
		if !capture.OK {
			t.Fatal("expected OK=true")
		}
		if capture.Content != "line1\nline2" {
			t.Fatalf("got content %q", capture.Content)
		}
	})

	t.Run("multiple sessions are demultiplexed", func(t *testing.T) {
		input := mk("main", "alpha\n") + mk("session-2", "beta\nbeta2\n") + mk("hex-abc", "")
		sessions, _, _ := ParseCaptureOutput(input)
		if len(sessions) != 3 {
			t.Fatalf("expected 3 sessions, got %d", len(sessions))
		}
		if sessions["main"].Content != "alpha" {
			t.Fatalf("main content: %q", sessions["main"].Content)
		}
		if sessions["session-2"].Content != "beta\nbeta2" {
			t.Fatalf("session-2 content: %q", sessions["session-2"].Content)
		}
		if sessions["hex-abc"].Content != "" {
			t.Fatalf("hex-abc should be empty, got %q", sessions["hex-abc"].Content)
		}
	})

	t.Run("session names with special characters survive", func(t *testing.T) {
		input := mk("fleet~abc123~def", "content")
		sessions, _, _ := ParseCaptureOutput(input)
		capture, ok := sessions["fleet~abc123~def"]
		if !ok {
			t.Fatalf("expected fleet~abc123~def in result; got keys: %v", sessionKeys(sessions))
		}
		if capture.Content != "content" {
			t.Fatalf("got content %q", capture.Content)
		}
	})

	t.Run("output without markers is ignored", func(t *testing.T) {
		sessions, files, _ := ParseCaptureOutput("orphan content with no marker\n")
		if len(sessions) != 0 || len(files) != 0 {
			t.Fatalf("expected empty results, got sessions=%d files=%d", len(sessions), len(files))
		}
	})
}

// ===========================================
// ParseCaptureOutput — files
// ===========================================

func TestParseCaptureOutput_Files(t *testing.T) {
	mkFile := func(path, content string) string {
		return fileMarker + path + "\n" + content
	}
	mkSession := func(name, content string) string {
		return sessionMarker + name + "\n" + content
	}

	t.Run("single file capture", func(t *testing.T) {
		input := mkFile("/tmp/fleet-man/claude-state", "working 1700000000\n")
		_, files, _ := ParseCaptureOutput(input)
		if got := files["/tmp/fleet-man/claude-state"]; got != "working 1700000000" {
			t.Errorf("got %q, want %q", got, "working 1700000000")
		}
	})

	t.Run("multiple files demultiplexed", func(t *testing.T) {
		input := mkFile("/tmp/fleet-man/claude-state", "working 100\n") +
			mkFile("/tmp/fleet-man/codex-state", "waiting 200\n")
		_, files, _ := ParseCaptureOutput(input)
		if len(files) != 2 {
			t.Fatalf("got %d files, want 2", len(files))
		}
		if files["/tmp/fleet-man/claude-state"] != "working 100" {
			t.Errorf("claude-state: %q", files["/tmp/fleet-man/claude-state"])
		}
		if files["/tmp/fleet-man/codex-state"] != "waiting 200" {
			t.Errorf("codex-state: %q", files["/tmp/fleet-man/codex-state"])
		}
	})

	t.Run("empty file content is captured as empty string", func(t *testing.T) {
		// State files exist but were empty (race between mkdir and
		// first hook fire) — must produce an entry, not an absence.
		input := mkFile("/tmp/fleet-man/claude-state", "")
		_, files, _ := ParseCaptureOutput(input)
		if _, present := files["/tmp/fleet-man/claude-state"]; !present {
			t.Fatalf("expected entry for empty file, got: %v", files)
		}
	})

	t.Run("sessions and files coexist in one output", func(t *testing.T) {
		// This is the normal CaptureAllScript shape: sessions first,
		// then state files. Both must demultiplex cleanly.
		input := mkSession("main", "tmux pane content\n") +
			mkFile("/tmp/fleet-man/claude-state", "working 42\n")
		sessions, files, _ := ParseCaptureOutput(input)
		if got := sessions["main"].Content; got != "tmux pane content" {
			t.Errorf("session content: %q", got)
		}
		if got := files["/tmp/fleet-man/claude-state"]; got != "working 42" {
			t.Errorf("file content: %q", got)
		}
	})

	t.Run("file content survives newlines and special chars", func(t *testing.T) {
		body := "line1\nline2\n  indented\n"
		input := mkFile("/tmp/fleet-man/claude-state", body)
		_, files, _ := ParseCaptureOutput(input)
		if got := files["/tmp/fleet-man/claude-state"]; got != "line1\nline2\n  indented" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("file marker followed by session marker switches mode", func(t *testing.T) {
		input := mkFile("/tmp/fleet-man/claude-state", "working 1\n") +
			mkSession("main", "pane\n")
		sessions, files, _ := ParseCaptureOutput(input)
		if files["/tmp/fleet-man/claude-state"] != "working 1" {
			t.Errorf("file content: %q", files["/tmp/fleet-man/claude-state"])
		}
		if sessions["main"].Content != "pane" {
			t.Errorf("session content: %q", sessions["main"].Content)
		}
	})
}

// ===========================================
// ParseCaptureOutput — hook-missing marker
// ===========================================

func TestParseCaptureOutput_HookMissing(t *testing.T) {
	mkSession := func(name, content string) string {
		return sessionMarker + name + "\n" + content
	}
	mkFile := func(path, content string) string {
		return fileMarker + path + "\n" + content
	}

	t.Run("marker alone sets hookMissing=true", func(t *testing.T) {
		_, _, hookMissing := ParseCaptureOutput(hookMissingMarker + "\n")
		if !hookMissing {
			t.Fatal("expected hookMissing=true")
		}
	})

	t.Run("marker absent sets hookMissing=false", func(t *testing.T) {
		input := mkSession("main", "pane\n") + mkFile("/tmp/fleet-man/claude-state", "working 1\n")
		_, _, hookMissing := ParseCaptureOutput(input)
		if hookMissing {
			t.Fatal("expected hookMissing=false when marker absent")
		}
	})

	t.Run("marker after sessions and files does not corrupt them", func(t *testing.T) {
		input := mkSession("main", "pane\n") +
			mkFile("/tmp/fleet-man/claude-state", "working 1\n") +
			hookMissingMarker + "\n"
		sessions, files, hookMissing := ParseCaptureOutput(input)
		if !hookMissing {
			t.Fatal("expected hookMissing=true")
		}
		if sessions["main"].Content != "pane" {
			t.Errorf("session content: %q", sessions["main"].Content)
		}
		if files["/tmp/fleet-man/claude-state"] != "working 1" {
			t.Errorf("file content: %q", files["/tmp/fleet-man/claude-state"])
		}
	})
}

func sessionKeys(captures map[string]ScreenCapture) []string {
	keys := make([]string, 0, len(captures))
	for key := range captures {
		keys = append(keys, key)
	}
	return keys
}
