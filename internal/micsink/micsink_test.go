package micsink

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCountRecordersOnlyCountsTheFleetMicrophone(t *testing.T) {
	sources := "0\tfleetnull.monitor\tmodule-null-sink.c\ts16le 2ch 44100Hz\tIDLE\n" +
		"1\tfleetmic\tmodule-pipe-source.c\ts16le 1ch 16000Hz\tRUNNING\n"
	outputs := "4\t1\t12\tprotocol-native.c\ts16le 1ch 16000Hz\n" + // arecord on the mic
		"5\t0\t13\tprotocol-native.c\ts16le 2ch 44100Hz\n" + // something on the monitor
		"6\t1\t14\tprotocol-native.c\tfloat32le 1ch 48000Hz\n"
	if got := countRecorders(sources, outputs); got != 2 {
		t.Fatalf("countRecorders = %d, want 2 (the monitor's recorder is not demand)", got)
	}
	if got := countRecorders(sources, ""); got != 0 {
		t.Fatalf("no outputs: got %d", got)
	}
	if got := countRecorders("0\tsomething-else\tx\n", outputs); got != 0 {
		t.Fatalf("no fleet microphone: got %d", got)
	}
}

func TestServerScriptDeclaresThePipeSourceAsDefault(t *testing.T) {
	script := serverScript()
	for _, want := range []string{
		"module-native-protocol-unix socket=" + SocketPath,
		"module-pipe-source source_name=fleetmic file=" + FIFOPath + " format=s16le rate=16000 channels=1",
		"set-default-source fleetmic",
		"module-null-sink",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("server script missing %q:\n%s", want, script)
		}
	}
}

func TestEnsureReportsMissingDeps(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPath = orig })
	if err := Ensure(); !errors.Is(err, ErrMissingDeps) {
		t.Fatalf("Ensure = %v, want ErrMissingDeps", err)
	}
	if err := Stop(); err != nil {
		t.Fatalf("Stop with nothing installed must be a no-op, got %v", err)
	}
}

func newTestFIFO(t *testing.T) (*fifo, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pcm")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	pipe, err := openFIFO(path)
	if err != nil {
		t.Fatalf("openFIFO: %v", err)
	}
	t.Cleanup(pipe.close)
	pipe.setOpen(true)
	return pipe, path
}

// readAvailable drains whatever is in the FIFO right now through a second fd.
func readAvailable(t *testing.T, path string) []byte {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer syscall.Close(fd)
	var out []byte
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if n <= 0 || err != nil {
			return out
		}
		out = append(out, buf[:n]...)
	}
}

// A read that splits a 16-bit sample must not shift every later sample by a
// byte — that turns speech into noise for the rest of the recording.
func TestFIFOKeepsSamplesAligned(t *testing.T) {
	pipe, path := newTestFIFO(t)
	pipe.write([]byte{1, 2, 3})
	if got := readAvailable(t, path); !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("after an odd write the pipe holds %v, want the whole samples only", got)
	}
	pipe.write([]byte{4, 5, 6})
	if got := readAvailable(t, path); !bytes.Equal(got, []byte{3, 4, 5, 6}) {
		t.Fatalf("carry not prefixed: %v", got)
	}
}

// With nobody reading, a microphone drops audio; it must never block the sink
// (which would back up the daemon) and never deliver a torn sample.
func TestFIFODropsWhenFullWithoutBlocking(t *testing.T) {
	pipe, path := newTestFIFO(t)
	big := bytes.Repeat([]byte{7, 9}, 4*1024*1024/2)
	done := make(chan struct{})
	go func() { pipe.write(big); close(done) }()
	<-done // would hang forever if write blocked on the full pipe

	got := readAvailable(t, path)
	if len(got) == 0 || len(got) >= len(big) {
		t.Fatalf("pipe holds %d of %d bytes; want a full pipe's worth and the rest dropped", len(got), len(big))
	}
	if len(got)%2 != 0 {
		t.Fatalf("pipe holds %d bytes: a sample was torn", len(got))
	}
}

// What is left in the pipe when a recording ends must not open the next one.
func TestFIFOClosingDiscardsStaleAudio(t *testing.T) {
	pipe, path := newTestFIFO(t)
	pipe.write([]byte("stale audio!"))
	pipe.write([]byte{1}) // the low half of a sample whose high half comes next
	pipe.setOpen(false)
	if got := readAvailable(t, path); len(got) != 0 {
		t.Fatalf("closing left %q in the pipe", got)
	}
	pipe.setOpen(true)
	pipe.write([]byte{2, 3})
	// No stale AUDIO — but the phase byte survives: {1,2} is one whole sample of
	// the stream, and 3 waits for its other half.
	if got := readAvailable(t, path); !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("after reopening the pipe holds %v, want the stream's next whole sample [1 2]", got)
	}
}

// The gate and the drain are one critical section: a write that arrives after
// the microphone closed — the pump always has a buffer in flight when the
// recorder leaves — must not repopulate the just-drained pipe.
func TestFIFOWriteWhileClosedIsDropped(t *testing.T) {
	pipe, path := newTestFIFO(t)
	pipe.setOpen(false)
	pipe.write([]byte("the tail of the last sentence"))
	if got := readAvailable(t, path); len(got) != 0 {
		t.Fatalf("a closed microphone accepted %q", got)
	}
}

// Hammer open/close against a writer: after the final close the pipe is empty,
// however the two interleaved.
func TestFIFOCloseAlwaysLeavesThePipeEmpty(t *testing.T) {
	pipe, path := newTestFIFO(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				pipe.write([]byte("audio audio audio audio "))
			}
		}
	}()
	for range 200 {
		pipe.setOpen(true)
		pipe.setOpen(false)
		if got := readAvailable(t, path); len(got) != 0 {
			close(stop)
			<-done
			t.Fatalf("%d stale bytes survived a close", len(got))
		}
	}
	close(stop)
	<-done
}

func TestOpenFIFOMissing(t *testing.T) {
	if _, err := openFIFO(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

// sampleStream yields an endless s16le stream whose every sample is (0xAA low,
// 0xBB high), so a phase slip is unmistakable: the pipe must only ever contain
// AA BB AA BB …, never BB AA ….
type sampleStream struct{ offset int }

func (s *sampleStream) next(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		if (s.offset+i)%2 == 0 {
			out[i] = 0xAA
		} else {
			out[i] = 0xBB
		}
	}
	s.offset += n
	return out
}

func requireInPhase(t *testing.T, what string, got []byte) {
	t.Helper()
	if len(got)%2 != 0 {
		t.Fatalf("%s: %d bytes in the pipe — a sample was torn", what, len(got))
	}
	for i := 0; i+1 < len(got); i += 2 {
		if got[i] != 0xAA || got[i+1] != 0xBB {
			t.Fatalf("%s: sample %d is % x, want aa bb — the stream lost its phase (byte-swapped audio is loud noise)", what, i/2, got[i:i+2])
		}
	}
}

// The carry byte is the stream's PHASE. Whatever happens to the audio — a buffer
// dropped because the pipe is full, a closed microphone discarding odd-sized
// chunks, a drain — the bytes that do reach the pipe must still be whole samples
// of the original stream. (An earlier "fix" cleared the carry on a drop: that
// makes the gap ODD, and every later sample comes out byte-swapped.)
func TestFIFOPhaseSurvivesDropsClosesAndDrains(t *testing.T) {
	pipe, path := newTestFIFO(t)
	stream := &sampleStream{}

	// Odd-sized chunks, as they really arrive (a pipe read and every hop after it
	// may split anywhere).
	for _, n := range []int{3, 5, 1, 7} {
		pipe.write(stream.next(n))
	}
	requireInPhase(t, "odd-sized chunks", readAvailable(t, path))

	// A drop with a carry pending: odd length, far larger than the pipe.
	pipe.write(stream.next(4*1024*1024 + 1))
	requireInPhase(t, "what fit before the drop", readAvailable(t, path))
	pipe.write(stream.next(6))
	got := readAvailable(t, path)
	if len(got) == 0 {
		t.Fatal("nothing written after the drop")
	}
	requireInPhase(t, "the buffer after a drop", got)

	// Closed: odd-sized chunks are discarded, but the phase is still tracked.
	pipe.setOpen(false)
	for _, n := range []int{5, 3, 9} {
		pipe.write(stream.next(n))
	}
	if got := readAvailable(t, path); len(got) != 0 {
		t.Fatalf("a closed microphone accepted %d bytes", len(got))
	}
	pipe.setOpen(true)
	pipe.write(stream.next(8))
	requireInPhase(t, "the first buffer of the NEXT recording", readAvailable(t, path))

	// …and a drain (which setOpen(false) performs) does not disturb it either.
	pipe.write(stream.next(5))
	pipe.setOpen(false)
	pipe.setOpen(true)
	pipe.write(stream.next(7))
	requireInPhase(t, "after a drain with a carry pending", readAvailable(t, path))
}

// A sink that exits mid-recording must not leave captured speech in the pipe.
func TestFIFOCloseDrains(t *testing.T) {
	pipe, path := newTestFIFO(t)
	pipe.write([]byte("the last words of a recording"))
	keep, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0) // keeps the pipe's buffer alive across close
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(keep)
	pipe.close()
	buf := make([]byte, 64)
	if n, _ := syscall.Read(keep, buf); n > 0 {
		t.Fatalf("close left %q in the pipe", buf[:n])
	}
}
