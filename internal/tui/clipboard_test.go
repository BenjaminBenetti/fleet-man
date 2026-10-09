package tui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClipboardCmdsWayland(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin always returns pbcopy")
	}
	got := clipboardCmdsFor("linux", true, false)
	want := [][]string{{"wl-copy"}, {"wl-copy", "--primary"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clipboardCmds() with WAYLAND_DISPLAY = %q, want %q", got, want)
	}
}

func TestClipboardCmdsX11Fallback(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin always returns pbcopy")
	}
	got := clipboardCmdsFor("linux", false, false)
	want := [][]string{{"xclip", "-sel", "clip", "-i"}, {"xclip", "-sel", "primary", "-i"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clipboardCmds() without WAYLAND_DISPLAY = %q, want %q", got, want)
	}
}

func TestClipboardCmdsWSL(t *testing.T) {
	got := clipboardCmdsFor("linux", false, true)
	want := [][]string{{"powershell.exe", "-NoProfile", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clipboardCmds() on WSL = %q, want %q", got, want)
	}
}

func TestNewClipboardSyncNoTmux(t *testing.T) {
	// With a non-existent tmux path, newClipboardSync should return nil.
	t.Setenv("PATH", "/nonexistent")
	cs := newClipboardSync()
	if cs != nil {
		t.Error("newClipboardSync() should return nil when tmux is not found")
	}
}

// TestClipboardHelperProcess runs in a subprocess so the tests exercise real
// exec cancellation and stdin pipes without touching the desktop clipboard or
// the developer's tmux server. The fake tmux executable execs this same helper.
func TestClipboardHelperProcess(t *testing.T) {
	if os.Getenv("FLEET_CLIPBOARD_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		mode, path := os.Args[i+1], os.Args[i+2]
		must := func(err error) {
			if err != nil {
				os.Exit(2)
			}
		}
		attempts, err := os.OpenFile(path+".attempts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		must(err)
		_, err = attempts.WriteString("x")
		must(err)
		must(attempts.Close())
		must(os.WriteFile(path+".started", nil, 0o600))
		if _, err := os.Stat(path + ".hang"); err == nil {
			// Deliberately leave stdin unread, including for large selections.
			time.Sleep(time.Minute)
			os.Exit(3)
		}
		if _, err := os.Stat(path + ".fail"); err == nil {
			os.Exit(1)
		}
		switch mode {
		case "tmux":
			args := os.Args[i+3:]
			if len(args) > 0 && args[0] == "display-message" {
				name, err := os.ReadFile(path + ".name")
				must(err)
				_, err = os.Stdout.Write(append(name, '\n'))
				must(err)
				os.Exit(0)
			}
			data, err := os.ReadFile(path)
			must(err)
			_, err = os.Stdout.Write(data)
			must(err)
		case "copy":
			data, err := io.ReadAll(os.Stdin)
			must(err)
			must(os.WriteFile(path, data, 0o600))
		default:
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func clipboardTestWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clipboardTestRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func clipboardTestRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func clipboardTestSync(t *testing.T) (cs *clipboardSync, buffer string, destinations []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux executable requires a POSIX shell")
	}
	dir := t.TempDir()
	buffer = filepath.Join(dir, "buffer")
	clipboardTestWrite(t, buffer+".name", "buffer0")
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_CLIPBOARD_TEST_HELPER", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nexec " + shQuote(helper) + " -test.run=^TestClipboardHelperProcess$ -- tmux " + shQuote(buffer) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cs = &clipboardSync{}
	for _, name := range []string{"regular", "primary"} {
		path := filepath.Join(dir, name)
		destinations = append(destinations, path)
		cs.clipCmds = append(cs.clipCmds, []string{helper, "-test.run=^TestClipboardHelperProcess$", "--", "copy", path})
	}
	return cs, buffer, destinations
}

func TestClipboardPollRecoversFromHungWriter(t *testing.T) {
	for _, blocked := range []int{0, 1} {
		t.Run([]string{"regular", "primary"}[blocked], func(t *testing.T) {
			cs, buffer, destinations := clipboardTestSync(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			clipboardTestWrite(t, buffer, "initial")
			cs.poll(ctx)

			// Exceed pipe capacity: cancellation must also release the stdin
			// copying goroutine when the stuck tool never reads its input.
			text := strings.Repeat("new selection\n", 16000)
			clipboardTestWrite(t, buffer, text)
			clipboardTestWrite(t, destinations[blocked]+".hang", "")
			started := time.Now()
			cs.poll(ctx)
			if elapsed := time.Since(started); elapsed > clipboardCommandTimeout+2*time.Second || ctx.Err() != nil {
				t.Fatalf("poll did not recover using its own timeout: elapsed=%v, parent=%v", elapsed, ctx.Err())
			}
			if got := clipboardTestRead(t, destinations[blocked]); got != "initial" {
				t.Fatal("hung destination unexpectedly changed")
			}
			if got := clipboardTestRead(t, destinations[1-blocked]); got != text {
				t.Fatal("hung writer prevented the other clipboard from updating")
			}

			clipboardTestRemove(t, destinations[blocked]+".hang")
			cs.poll(ctx) // Retry the same text without restarting the synchronizer.
			if got := clipboardTestRead(t, destinations[blocked]); got != text {
				t.Fatal("timed-out write was not retried")
			}
			if got := clipboardTestRead(t, destinations[1-blocked]+".attempts"); got != "xx" {
				t.Fatalf("successful destination was unnecessarily recopied: attempts=%q", got)
			}

			clipboardTestWrite(t, buffer, "next selection")
			cs.poll(ctx)
			for _, path := range destinations {
				if got := clipboardTestRead(t, path); got != "next selection" {
					t.Fatalf("later copy failed for %s: %q", filepath.Base(path), got)
				}
			}
		})
	}
}

func TestClipboardPollRetriesOnlyFailedDestination(t *testing.T) {
	cs, buffer, destinations := clipboardTestSync(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clipboardTestWrite(t, buffer, "retry me")
	clipboardTestWrite(t, destinations[1]+".fail", "")
	cs.poll(ctx)
	clipboardTestRemove(t, destinations[1]+".fail")
	cs.poll(ctx)
	cs.poll(ctx)
	for i, path := range destinations {
		if got := clipboardTestRead(t, path); got != "retry me" {
			t.Fatalf("destination %d was not updated: %q", i, got)
		}
		if got, want := clipboardTestRead(t, path+".attempts"), []string{"x", "xx"}[i]; got != want {
			t.Fatalf("destination %d attempts = %q, want %q", i, got, want)
		}
	}
}

func TestClipboardPollRecoversFromHungTmux(t *testing.T) {
	cs, buffer, destinations := clipboardTestSync(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clipboardTestWrite(t, buffer, "copied after tmux recovers")
	clipboardTestWrite(t, buffer+".hang", "")
	started := time.Now()
	cs.poll(ctx)
	if elapsed := time.Since(started); elapsed > clipboardCommandTimeout+2*time.Second || ctx.Err() != nil {
		t.Fatalf("tmux read did not time out: elapsed=%v, parent=%v", elapsed, ctx.Err())
	}
	clipboardTestRemove(t, buffer+".hang")
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != "copied after tmux recovers" {
			t.Fatalf("clipboard failed to recover: %q", got)
		}
	}
}

func TestClipboardStartCancelsHungWriter(t *testing.T) {
	cs, buffer, destinations := clipboardTestSync(t)
	clipboardTestWrite(t, buffer, "cancel me")
	clipboardTestWrite(t, destinations[0]+".hang", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); cs.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(destinations[0] + ".started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clipboard writer never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelling the UI did not promptly stop the hung clipboard writer")
	}
	if _, err := os.Stat(destinations[1] + ".started"); !os.IsNotExist(err) {
		t.Fatalf("second writer ran after cancellation: %v", err)
	}
}

func TestClipboardPollPreservesLargeText(t *testing.T) {
	cs, buffer, destinations := clipboardTestSync(t)
	text := strings.Repeat("quotes '\" $HOME `literal` $(literal)\n\x00", 8000)
	clipboardTestWrite(t, buffer, text)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != text {
			t.Fatalf("large clipboard text changed: got %d bytes, want %d", len(got), len(text))
		}
	}
}

func TestClipboardPollRecopiesIdenticalText(t *testing.T) {
	cs, buffer, destinations := clipboardTestSync(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clipboardTestWrite(t, buffer, "hello")
	cs.poll(ctx)
	for _, path := range destinations {
		clipboardTestWrite(t, path, "URL copied in browser")
	}
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != "URL copied in browser" {
			t.Fatalf("ordinary poll overwrote another app's clipboard: %q", got)
		}
	}
	// A new selection has a new tmux buffer name, even for identical text.
	clipboardTestWrite(t, buffer+".name", "buffer1")
	clipboardTestWrite(t, destinations[1]+".fail", "")
	cs.poll(ctx)
	if got := clipboardTestRead(t, destinations[0]); got != "hello" {
		t.Fatalf("explicit recopy left stale clipboard contents: %q", got)
	}
	clipboardTestRemove(t, destinations[1]+".fail")
	cs.poll(ctx)
	for i, path := range destinations {
		if got := clipboardTestRead(t, path); got != "hello" {
			t.Fatalf("destination %d failed to recopy identical text: %q", i, got)
		}
		if got, want := clipboardTestRead(t, path+".attempts"), []string{"xx", "xxx"}[i]; got != want {
			t.Fatalf("destination %d attempts = %q, want %q", i, got, want)
		}
	}
}

func TestClipboardPollRecopiesIdenticalTextWithTmux(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux required for integration test")
	}
	cs, buffer, destinations := clipboardTestSync(t)
	// A short, explicit socket path isolates the test from any existing tmux
	// server and fits macOS's Unix socket path limit.
	dir, err := os.MkdirTemp("/tmp", "fleetclip-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	socket := filepath.Join(dir, "socket")
	tmux := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, realTmux, append([]string{"-S", socket, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	tmux("new-session", "-d", "-s", "clipboard-test", "sleep 60")
	t.Cleanup(func() { tmux("kill-server") })
	wrapper := "#!/bin/sh\nexec " + shQuote(realTmux) + " -S " + shQuote(socket) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(buffer), "tmux"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs.poll(ctx) // No buffers yet.
	tmux("set-buffer", "hello")
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != "hello" {
			t.Fatalf("initial copy = %q", got)
		}
		clipboardTestWrite(t, path, "URL copied in browser")
	}
	// Middle-click paste uses a temporary named buffer. Seeing it appear
	// and disappear must not be mistaken for a new copy from Fleet.
	tmux("set-buffer", "-b", "__fleet_primary", "temporary paste")
	cs.poll(ctx)
	tmux("delete-buffer", "-b", "__fleet_primary")
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != "URL copied in browser" {
			t.Fatalf("polling or middle-click paste overwrote external clipboard: %q", got)
		}
	}
	tmux("set-buffer", "hello") // Fresh auto-named buffer, identical text.
	cs.poll(ctx)
	for _, path := range destinations {
		if got := clipboardTestRead(t, path); got != "hello" {
			t.Fatalf("real tmux recopy left stale clipboard contents: %q", got)
		}
	}
}
