package micsink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/output"
)

func TestOutputOnlyScriptHasNoMicrophone(t *testing.T) {
	if script := audioServerScript(false); strings.Contains(script, "module-pipe-source") || strings.Contains(script, "fleetmic") {
		t.Fatalf("output-only server creates microphone: %s", script)
	}
}

func TestOutputRequiresClientRoutingBeforeBecomingReady(t *testing.T) {
	old := outputClientConfig
	outputClientConfig = filepath.Join(t.TempDir(), "missing.conf")
	defer func() { outputClientConfig = old }()
	var out bytes.Buffer
	if err := RunOutput(context.Background(), strings.NewReader(""), &out); err == nil {
		t.Fatal("source came up before applications could discover the server")
	}
	if out.Len() != 0 {
		t.Fatal("unconfigured source announced ready")
	}
}

// Exercise a real private PulseAudio server, without sound hardware or Docker:
// generate a tone, play it into the virtual sink and receive actual PCM through
// the same source command the daemon runs in an instance.
func TestOutputPulseRoundTripAndIndependentToggles(t *testing.T) {
	for _, bin := range []string{"pulseaudio", "pactl", "paplay", "parec"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	oldDir := dir
	dir = t.TempDir()
	oldConfig := outputClientConfig
	outputClientConfig = filepath.Join(dir, "client.conf")
	if err := os.WriteFile(outputClientConfig, []byte("default-server = unix:"+SocketPath+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopServer(); dir = oldDir })
	t.Cleanup(func() { outputClientConfig = oldConfig })
	if err := EnsureOutput(); err != nil {
		t.Fatal(err)
	}
	if micPresent() != no {
		t.Fatal("output-only setup exposed a microphone")
	}
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	if micPresent() != yes {
		t.Fatal("microphone not restored alongside output")
	}
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if ok, err := outputPresent(); err != nil || !ok {
		t.Fatalf("disabling microphone interrupted output: %v %v", ok, err)
	}
	if micPresent() != no {
		t.Fatal("disabled microphone still present")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inR, inW := io.Pipe()
	defer inR.Close()
	defer inW.Close()
	outR, outW := io.Pipe()
	defer outR.Close()
	done := make(chan error, 1)
	go func() { err := RunOutput(ctx, inR, outW); outW.Close(); done <- err }()
	reader := bufio.NewReader(outR)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("source greeting %q %v", line, err)
	}
	// Start reading before paplay to avoid blocking the source's stdout.
	heard := make(chan bool, 1)
	go func() {
		buf := make([]byte, output.ChunkBytes)
		for {
			if _, err := io.ReadFull(reader, buf); err != nil {
				heard <- false
				return
			}
			for _, b := range buf {
				if b != 0 {
					heard <- true
					_, _ = io.Copy(io.Discard, reader)
					return
				}
			}
		}
	}()
	pcm := make([]byte, output.SampleRate*4/5)
	for i := 0; i < len(pcm); i += 4 {
		v := int16(12000 * math.Sin(2*math.Pi*440*float64(i/4)/output.SampleRate))
		binary.LittleEndian.PutUint16(pcm[i:], uint16(v))
		binary.LittleEndian.PutUint16(pcm[i+2:], uint16(v))
	}
	cmd := exec.CommandContext(ctx, "paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--device="+output.VirtualSink)
	cmd.Env = pulseEnv()
	cmd.Stdin = bytes.NewReader(pcm)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("play tone: %v %s", err, out)
	}
	select {
	case ok := <-heard:
		if !ok {
			t.Fatal("source never returned audio")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for tone")
	}
	// Stdin EOF, not just a local process kill, must stop the remote recorder.
	inW.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source ignored daemon disconnect")
	}
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := StopOutput(); err != nil {
		t.Fatal(err)
	}
	if micPresent() != yes {
		t.Fatal("disabling output interrupted microphone")
	}
	if ok, err := outputPresent(); err != nil || ok {
		t.Fatalf("output sink survived disable: %v %v", ok, err)
	}
}
