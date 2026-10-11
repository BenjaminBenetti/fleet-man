package micsink

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// counted is n bytes of s16le samples numbered from first: which audio a
// pipe kept is then readable from the samples themselves.
func counted(first, n int) []byte {
	out := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		binary.LittleEndian.PutUint16(out[i:], uint16(first+i/2))
	}
	return out
}

// Audio that arrives in a burst must not sit in the pipe as latency for the
// rest of the recording: past backlogMax the OLDEST goes, down to
// backlogKeep, and what is left is the newest audio, whole samples, in order.
func TestFIFOCapsTheBacklogToTheNewestAudio(t *testing.T) {
	pipe, path := newTestFIFO(t)

	// One second at once (a stalled path delivering everything).
	pipe.write(counted(0, 32000))
	got := readAvailable(t, path)
	if want := counted(16000-backlogKeep/2, backlogKeep); !bytes.Equal(got, want) {
		t.Fatalf("after a 1s burst the pipe holds %d bytes starting at sample %d; want the newest %d bytes", len(got), firstSample(got), backlogKeep)
	}

	// 40 ms frames with nobody reading: the backlog grows to backlogMax, then
	// the oldest is discarded, and the pipe always ends with the newest frame.
	next := 0
	for i := range 20 {
		pipe.write(counted(next, 1280))
		next += 640
		_ = i
	}
	got = readAvailable(t, path)
	if len(got) > backlogMax || len(got) < backlogKeep {
		t.Fatalf("the backlog is %d bytes, want between %d and %d", len(got), backlogKeep, backlogMax)
	}
	if want := counted(next-len(got)/2, len(got)); !bytes.Equal(got, want) {
		t.Fatalf("the pipe holds samples from %d, want the newest %d bytes in order", firstSample(got), len(got))
	}

	// Under the cap nothing is discarded.
	pipe.write(counted(0, 1280))
	if got := readAvailable(t, path); !bytes.Equal(got, counted(0, 1280)) {
		t.Fatalf("a short backlog lost audio: %d bytes", len(got))
	}
}

func firstSample(b []byte) int {
	if len(b) < 2 {
		return -1
	}
	return int(binary.LittleEndian.Uint16(b))
}
