package tui

import (
	"strings"
	"testing"
)

func TestCopyTouchesHost(t *testing.T) {
	cases := []struct {
		src, dst string
		want     bool
	}{
		{"host:report.csv", "alpha:/tmp/", true}, // host source (upload from host)
		{"alpha:/out", "host:/here", true},       // host dest (download to host)
		{":out.bin", "", true},                   // download shorthand → host downloads
		{"plain.txt", "alpha:/tmp/", true},       // bare path treated as host (defensive)
		{":out", "other:/tmp/", false},           // self → another instance, no host path
		{"a:x", "b:y", false},                    // instance → instance
	}
	for _, tc := range cases {
		if got := copyTouchesHost(tc.src, tc.dst); got != tc.want {
			t.Errorf("copyTouchesHost(%q,%q) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}

func TestRequestCopyGating(t *testing.T) {
	m := &model{copySessionAllow: map[string]bool{}}

	// A copy purely between instances runs immediately, no prompt.
	if cmd := m.requestCopy(copyRequest{fleet: "f", instance: "i", src: ":a", dst: "other:/b"}); cmd == nil {
		t.Fatal("instance→instance copy should run immediately (non-nil cmd)")
	}
	if m.copyConfirmShowing() {
		t.Fatal("instance→instance copy should not queue a confirmation")
	}

	// A host-touching copy is queued for confirmation.
	if cmd := m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:secret.txt", dst: ":/tmp/"}); cmd != nil {
		t.Fatal("host-touching copy should queue (nil cmd), not run")
	}
	if !m.copyConfirmShowing() || len(m.pendingCopyConfirms) != 1 {
		t.Fatalf("host-touching copy should queue exactly one, got %d", len(m.pendingCopyConfirms))
	}

	// Once the instance is session-allowed, host-touching copies run immediately.
	m.copySessionAllow["f/i"] = true
	if cmd := m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:x.txt", dst: ":/tmp/"}); cmd == nil {
		t.Fatal("session-allowed host copy should run immediately")
	}
	if len(m.pendingCopyConfirms) != 1 {
		t.Fatalf("session-allowed copy should not enqueue; pending = %d", len(m.pendingCopyConfirms))
	}
}

func TestResolveCopyConfirmAllowOnce(t *testing.T) {
	m := &model{copySessionAllow: map[string]bool{}}
	m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:a.txt", dst: ":/tmp/"})

	if cmd := m.resolveCopyConfirm("a"); cmd == nil {
		t.Fatal("allow-once should return a copy command")
	}
	if m.copyConfirmShowing() {
		t.Fatal("queue should be empty after allow-once")
	}
	if m.copySessionAllow["f/i"] {
		t.Fatal("allow-once must NOT set the session allowance")
	}
}

func TestResolveCopyConfirmSessionDrainsSameInstance(t *testing.T) {
	m := &model{copySessionAllow: map[string]bool{}}
	m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:a.txt", dst: ":/tmp/"})
	m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:b.txt", dst: ":/tmp/"})
	m.requestCopy(copyRequest{fleet: "f", instance: "j", src: "host:c.txt", dst: ":/tmp/"})

	if cmd := m.resolveCopyConfirm("s"); cmd == nil {
		t.Fatal("session allow should return command(s) to run")
	}
	if !m.copySessionAllow["f/i"] {
		t.Fatal("session allow must record the instance")
	}
	// Both f/i requests cleared; the f/j request stays queued for its own prompt.
	if len(m.pendingCopyConfirms) != 1 || m.pendingCopyConfirms[0].instanceKey() != "f/j" {
		t.Fatalf("only the other instance's request should remain, got %+v", m.pendingCopyConfirms)
	}
}

func TestResolveCopyConfirmDeny(t *testing.T) {
	m := &model{copySessionAllow: map[string]bool{}}
	m.requestCopy(copyRequest{fleet: "f", instance: "i", src: "host:a.txt", dst: ":/tmp/"})

	if cmd := m.resolveCopyConfirm("d"); cmd != nil {
		t.Fatal("deny should not return a copy command")
	}
	if m.copyConfirmShowing() {
		t.Fatal("queue should be empty after deny")
	}
	if !strings.Contains(m.message, "Denied") {
		t.Fatalf("deny should report it; message = %q", m.message)
	}
}

// TestCopyConfirmOpenRequest covers the `fleet open` flavour of a delegated
// request: it is always host-touching (so gated), the prompt names the open as
// an extra effect and titles itself as an open, and the status lines say
// "open" rather than "copy".
func TestCopyConfirmOpenRequest(t *testing.T) {
	m := &model{copySessionAllow: map[string]bool{}}
	req := copyRequest{fleet: "f", instance: "i", src: ":out/chart.png", open: true}
	if cmd := m.requestCopy(req); cmd != nil || !m.copyConfirmShowing() {
		t.Fatal("an open always touches the host, so it must queue for confirmation")
	}

	effects := strings.Join(copyConfirmHostEffects(req), "\n")
	if !strings.Contains(effects, "downloads folder") || !strings.Contains(effects, "opens it") {
		t.Errorf("effects should name the write AND the open, got %q", effects)
	}
	if plain := strings.Join(copyConfirmHostEffects(copyRequest{src: ":x", dst: ""}), "\n"); strings.Contains(plain, "opens it") {
		t.Errorf("a plain copy must not claim to open anything, got %q", plain)
	}
	if view := m.viewCopyConfirm(); !strings.Contains(view, "Open request from f/i") {
		t.Errorf("open prompt should be titled as an open request, got %q", view)
	}

	m.resolveCopyConfirm("d")
	if !strings.Contains(m.message, "Denied open") {
		t.Errorf("deny should say open, got %q", m.message)
	}

	m.requestCopy(req)
	if cmd := m.resolveCopyConfirm("a"); cmd == nil {
		t.Fatal("allow should start the copy+open")
	}
	if !strings.Contains(m.message, "and opening") {
		t.Errorf("status should announce the open, got %q", m.message)
	}
}
