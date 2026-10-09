package micsink

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputPulseCleanupAfterPersistentFailure(t *testing.T) {
	routingPulse(t)
	id, pcm, disconnect := routingOutput(t)
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	playback := routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
		"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
	realPactl, err := exec.LookPath("pactl")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$*\" = 'set-default-source fleetnull.monitor' ]; then exit 1; fi\nexec %q \"$@\"\n", realPactl)
	if err := os.WriteFile(filepath.Join(bin, "pactl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for range 3 { // all daemon retries fail; preserve the same live output
		if err := Stop(); err == nil || micPresent() != yes {
			t.Fatalf("expected retained microphone and failure: %v", err)
		}
		assertRoutingOutput(t, id)
		assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
	}
	disconnect()
	playback()
	if err := StopOutput(); err != nil {
		t.Fatal(err)
	}
	// Daemon output teardown repeats mic cleanup. With output gone this works
	// even while the fallback command is STILL broken.
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for serverAnswers() != no && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if serverAnswers() != no {
		t.Fatal("both features off left the sound server running")
	}
}

func TestOutputPulseLateCleanupPreservesReenabledMic(t *testing.T) {
	routingPulse(t)
	id, pcm, _ := routingOutput(t)
	routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
		"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
	stopMic := routingMic(t)
	micPCM := &routingPCM{}
	stopRecorder := routingCommand(t, nil, micPCM, "parec", "--raw", "--format=s16le",
		"--rate=16000", "--channels=1", "--latency-msec=20")
	assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
	for range 3 {
		started := time.Now()
		if err := Stop(); err == nil || !strings.Contains(err.Error(), "feed still active") {
			t.Fatalf("late cleanup must defer to active feed: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("active feed blocked cleanup for %v", elapsed)
		}
		assertRoutingOutput(t, id)
		assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
		assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
	}
	stopRecorder()
	stopMic()
	if err := Stop(); err != nil {
		t.Fatalf("closed feed still blocked cleanup: %v", err)
	}
	if micPresent() != no {
		t.Fatal("microphone survived cleanup after feed closed")
	}
	if _, err := pactl(context.Background(), "info"); err != nil {
		t.Fatalf("mic cleanup stopped active output server: %v", err)
	}
	assertRoutingOutput(t, id)
}

func TestOutputPulseCleanupWaitsForSinkExit(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm-output=%v", warm), func(t *testing.T) {
			routingPulse(t)
			var id string
			var pcm *routingPCM
			if warm {
				id, pcm, _ = routingOutput(t)
				routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
					"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
			}
			sink := startSink(t)
			if warm {
				sink.expect(t, EventReady)
			} else {
				// Catch cold startup after Run acquired audio.lock, without
				// waiting for its ready event or for its eventual shutdown.
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(path("mic-feed.lock")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("sink never started")
					}
					time.Sleep(time.Millisecond)
				}
			}
			// Match the daemon: close stdin, then start cleanup immediately.
			// Unlike stopMic(), this does NOT await Run's return first.
			sink.stdin.Close()
			if err := Stop(); err != nil {
				t.Fatalf("ordinary shutdown required another daemon retry: %v", err)
			}
			select {
			case err := <-sink.done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cleanup finished before the old feed released its lease")
			}
			if warm {
				if micPresent() != no {
					t.Fatal("mic-off retained the old microphone")
				}
				assertRoutingOutput(t, id)
				assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for serverAnswers() != no && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if serverAnswers() != no {
					t.Fatal("cold sink left the sound server running after mic-off")
				}
			}
		})
	}
}
