package micsink

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/output"
)

func TestOutputOnlyScriptHasNoMicrophone(t *testing.T) {
	if script := audioServerScript(false); strings.Contains(script, "module-pipe-source") || strings.Contains(script, "fleetmic") {
		t.Fatalf("output-only server creates microphone: %s", script)
	}
	if script := audioServerScript(false); !strings.Contains(script, "set-default-source fleetnull.monitor\n") {
		t.Fatalf("output-only server has no silent default source: %s", script)
	}
}

func TestOutputRequiresClientRoutingBeforeBecomingReady(t *testing.T) {
	old, oldUser := outputClientConfig, userClientConfig
	outputClientConfig = filepath.Join(t.TempDir(), "missing.conf")
	userClientConfig = func() string { return filepath.Join(os.TempDir(), "fleet-no-such-home", UserClientConfig) }
	defer func() { outputClientConfig, userClientConfig = old, oldUser }()
	var out bytes.Buffer
	if err := RunOutput(context.Background(), strings.NewReader(""), &out); err == nil {
		t.Fatal("source came up before applications could discover the server")
	}
	if out.Len() != 0 {
		t.Fatal("unconfigured source announced ready")
	}
}

// frames is the source's PCM as a reader: n frames of each level in turn.
func outputFrames(levels ...int16) []byte {
	var out []byte
	for _, v := range levels {
		frame := make([]byte, output.ChunkBytes)
		for i := 0; i < len(frame); i += 2 {
			binary.LittleEndian.PutUint16(frame[i:], uint16(v))
		}
		out = append(out, frame...)
	}
	return out
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) frames() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out [][]byte
	data := b.buf.Bytes()
	for len(data) >= output.ChunkBytes {
		out = append(out, append([]byte(nil), data[:output.ChunkBytes]...))
		data = data[output.ChunkBytes:]
	}
	return out
}

// Leading silence is not sent, an audible burst goes out whole with
// outputTail silent frames after it, and the silence after that is not sent
// either (dither counts as silence).
func TestOutputSendsOnlyAudibleFramesAndATail(t *testing.T) {
	orig := outputAliveInterval
	outputAliveInterval = time.Hour
	t.Cleanup(func() { outputAliveInterval = orig })
	var levels []int16
	for range 20 {
		levels = append(levels, 0)
	}
	levels = append(levels, 3000, 5, 3000) // a burst with a dithered frame inside
	for range 20 {
		levels = append(levels, 2) // dither: silence
	}
	levels = append(levels, -4000)
	out := &lockedBuffer{}
	if err := forwardOutput(context.Background(), bytes.NewReader(outputFrames(levels...)), out); err == nil {
		t.Fatal("forwardOutput returned nil at the end of its PCM")
	}
	got := out.frames()
	var loud []bool
	for _, f := range got {
		loud = append(loud, !output.Silent(f))
	}
	want := []bool{true, false, true, false, false, false, false, false, true}
	if len(loud) != len(want) {
		t.Fatalf("sent %d frames %v, want %d %v", len(loud), loud, len(want), want)
	}
	for i := range want {
		if loud[i] != want[i] {
			t.Fatalf("sent %v, want %v", loud, want)
		}
	}
}

// A quiet source still says it runs: a silent frame every
// outputAliveInterval, so the daemon's deadline holds only a dead one.
func TestOutputHeartbeatWhileQuiet(t *testing.T) {
	orig := outputAliveInterval
	outputAliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { outputAliveInterval = orig })
	pr, pw := io.Pipe()
	defer pw.Close()
	out := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- forwardOutput(ctx, pr, out) }()
	go func() {
		silence := outputFrames(0)
		for ctx.Err() == nil {
			if _, err := pw.Write(silence); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("forwardOutput = %v after its context ended", err)
	}
	beats := out.frames()
	if len(beats) < 4 || len(beats) > 12 {
		t.Fatalf("%d heartbeat frames in 500ms at a 50ms interval", len(beats))
	}
	for _, f := range beats {
		if !output.Silent(f) {
			t.Fatal("a heartbeat frame carried sound")
		}
	}
}

// Without root the setup script points the session user's own pulse clients
// at the server (UserClientConfig): that routes output as the system drop-in
// would.
func TestOutputAcceptsTheUsersOwnRouting(t *testing.T) {
	old, oldUser := outputClientConfig, userClientConfig
	defer func() { outputClientConfig, userClientConfig = old, oldUser }()
	outputClientConfig = filepath.Join(t.TempDir(), "missing.conf")
	home := t.TempDir()
	userClientConfig = func() string { return filepath.Join(home, UserClientConfig) }
	if outputConfigured() {
		t.Fatal("configured with no drop-in anywhere")
	}
	path := filepath.Join(home, UserClientConfig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("default-server = unix:/elsewhere.sock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if outputConfigured() {
		t.Fatal("a drop-in naming another server counts as fleet's routing")
	}
	if err := os.WriteFile(path, []byte("# managed by fleet\ndefault-server = unix:"+SocketPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !outputConfigured() {
		t.Fatal("the user's own drop-in was not taken")
	}
}
