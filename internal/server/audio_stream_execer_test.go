package server

import (
	"bufio"
	"io"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
)

// audio_stream_execer_test.go: the mic sink and the audio output source over a
// backend that runs commands as streams of its own (backend.StreamExecer).

// outputStreamExecer is a streamExecer that can host an audio source when
// supported is set.
type outputStreamExecer struct {
	streamExecer
	supported bool
	cmd       *exec.Cmd
}

func (b *outputStreamExecer) OutputSourceCommand(string) (*exec.Cmd, bool) {
	if !b.supported {
		return nil, false
	}
	b.cmd = exec.Command("false")
	return b.cmd, true
}

// requireAudioConn checks conn is proc's command as the hubs drive it: writes
// reach its stdin, its stdout is read back, its stderr never blocks it, and
// Close aborts and reaps it.
func requireAudioConn(t *testing.T, conn io.ReadWriteCloser, proc *fakeExecStream) {
	t.Helper()
	played := make(chan struct{})
	go func() {
		defer close(played)
		pcm := make([]byte, 4)
		_, _ = io.ReadFull(proc.cmdStdin, pcm)
		_, _ = proc.cmdStderr.Write([]byte("pulse: noise nobody reads\n"))
		_, _ = proc.cmdStdout.Write(append([]byte("ready "), pcm...))
	}()
	if _, err := conn.Write([]byte("pcm!")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('!')
	if err != nil || line != "ready pcm!" {
		t.Fatalf("read %q (%v), want the command's stdout after its stderr", line, err)
	}
	select {
	case <-played:
	case <-time.After(10 * time.Second):
		t.Fatal("the command's stderr was not drained")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_ = conn.Close()
	waitFor(t, "the command to be aborted and reaped once", func() bool {
		closes, waits := proc.counts()
		return closes == 1 && waits == 1
	})
}

// TestStartMicSinkPrefersStreamExecer: the sink runs in the backend's own
// stream, as the session user (ExecClassScript), by the staged binary's path,
// marked as an audio stream (it must fail closed). A backend that can't host a
// sink gets none.
func TestStartMicSinkPrefersStreamExecer(t *testing.T) {
	inst := &fleet.Instance{Name: "i1", WorkspaceDir: "/ws/f/i1", ContainerID: "c1"}
	b := &streamExecer{micSink: true}
	conn, ok, err := startMicSink(b, inst)
	if err != nil || !ok {
		t.Fatalf("startMicSink = %v, %v; want a sink", ok, err)
	}
	reqs, streams := b.opened()
	want := backend.ExecRequest{
		Class:        backend.ExecClassScript,
		WorkspaceDir: "/ws/f/i1",
		ContainerID:  "c1",
		Argv:         fleetlaunch.MicSinkArgv(),
		Audio:        true,
	}
	if len(reqs) != 1 || !reflect.DeepEqual(reqs[0], want) {
		t.Fatalf("OpenExec requests = %+v, want [%+v]", reqs, want)
	}
	requireAudioConn(t, conn, streams[0])

	b = &streamExecer{micSink: false}
	if conn, ok, err := startMicSink(b, inst); conn != nil || ok || err != nil {
		t.Fatalf("startMicSink without mic support = %v, %v, %v; want nothing", conn, ok, err)
	}
	if reqs, _ := b.opened(); len(reqs) != 0 {
		t.Fatalf("a sink was opened on a backend without mic support: %+v", reqs)
	}
}

// TestStartOutputSourcePrefersStreamExecer: the backend's OutputSourceCommand
// says whether the instance can have a source; a StreamExecer then runs it in
// its own stream instead of starting that host command.
func TestStartOutputSourcePrefersStreamExecer(t *testing.T) {
	inst := &fleet.Instance{Name: "i1", WorkspaceDir: "/ws/f/i1", ContainerID: "c1"}
	b := &outputStreamExecer{supported: true}
	conn, ok, err := startOutputSource(b, inst)
	if err != nil || !ok {
		t.Fatalf("startOutputSource = %v, %v; want a source", ok, err)
	}
	if b.cmd.Process != nil {
		t.Fatal("the host command was started as well")
	}
	reqs, streams := b.opened()
	want := backend.ExecRequest{
		Class:        backend.ExecClassScript,
		WorkspaceDir: "/ws/f/i1",
		ContainerID:  "c1",
		Argv:         fleetlaunch.OutputSourceArgv(),
		Audio:        true,
	}
	if len(reqs) != 1 || !reflect.DeepEqual(reqs[0], want) {
		t.Fatalf("OpenExec requests = %+v, want [%+v]", reqs, want)
	}
	requireAudioConn(t, conn, streams[0])

	b = &outputStreamExecer{supported: false}
	if conn, ok, err := startOutputSource(b, inst); conn != nil || ok || err != nil {
		t.Fatalf("startOutputSource without support = %v, %v, %v; want nothing", conn, ok, err)
	}
	if reqs, _ := b.opened(); len(reqs) != 0 {
		t.Fatalf("a source was opened on a backend without output support: %+v", reqs)
	}
}
