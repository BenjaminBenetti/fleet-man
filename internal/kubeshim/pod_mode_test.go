package kubeshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testContainer() Container {
	return Container{
		ContainerID: "k8s:cluster1/fleet/fi-app-main-1a2b3c4d@g1",
		Name:        "fi-app-main-1a2b3c4d-0",
		Image:       "registry/fleet/devcontainer@sha256:abc",
		ImageID:     "sha256:abc",
		Created:     time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		StartedAt:   StartedAt(time.Date(2026, 10, 2, 8, 30, 15, 0, time.UTC), "pod-uid", 0),
		User:        "root",
		Env:         []string{"PATH=/usr/bin:/bin", "SECRET=s3cr3t"},
		Labels:      map[string]string{"devcontainer.metadata": `[{"remoteUser":"vscode"}]`, "devcontainer.local_folder": "/ws"},
		WorkingDir:  "/",
		Entrypoint:  []string{"/bin/sh"},
		Cmd:         []string{"-c", "echo Container started", "-"},
	}
}

func TestPodModeInspect(t *testing.T) {
	c := testContainer()
	s := newTestShim(t, Config{Mode: ModePod, Pod: &PodConfig{Container: c}})
	for _, args := range [][]string{
		{"inspect", "--type", "container", c.ContainerID},
		{"inspect", c.ID()},
		{"container", "inspect", c.ID()[:12]},
		{"inspect", "--type=container", "/" + c.Name},
	} {
		code, out, errOut := s.docker(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %q", args, code, errOut)
		}
		var docs []map[string]any
		if err := json.Unmarshal([]byte(out), &docs); err != nil || len(docs) != 1 {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		d := docs[0]
		if d["Id"] != c.ID() || len(c.ID()) != 64 {
			t.Errorf("Id = %v", d["Id"])
		}
		if d["Created"] != "2026-10-01T12:00:00Z" {
			t.Errorf("Created = %v", d["Created"])
		}
		state := d["State"].(map[string]any)
		if state["Status"] != "running" || state["StartedAt"] != c.StartedAt.Format(time.RFC3339Nano) {
			t.Errorf("State = %v", state)
		}
		cfg := d["Config"].(map[string]any)
		if cfg["User"] != "root" || cfg["Labels"].(map[string]any)["devcontainer.metadata"] != c.Labels["devcontainer.metadata"] {
			t.Errorf("Config = %v", cfg)
		}
		if !reflect.DeepEqual(cfg["Env"], []any{"PATH=/usr/bin:/bin", "SECRET=s3cr3t"}) {
			t.Errorf("Env = %v", cfg["Env"])
		}
		if ports, ok := d["NetworkSettings"].(map[string]any)["Ports"].(map[string]any); !ok || len(ports) != 0 {
			t.Errorf("NetworkSettings.Ports must be an empty object, got %v", d["NetworkSettings"])
		}
		if d["Path"] != "/bin/sh" || d["Name"] != "/"+c.Name {
			t.Errorf("Path %v Name %v", d["Path"], d["Name"])
		}
	}

	code, out, errOut := s.docker("inspect", "--type", "container", c.ContainerID, "other")
	if code != 1 || !strings.Contains(errOut, "No such container: other") {
		t.Errorf("a missing name: exit %d %q", code, errOut)
	}
	var docs []any
	if json.Unmarshal([]byte(out), &docs) != nil || len(docs) != 1 {
		t.Errorf("the found container should still be printed: %s", out)
	}
	if code, _, errOut := s.docker("inspect", "--type", "image", c.Image); code != 1 || !strings.Contains(errOut, "No such image") {
		t.Errorf("inspect image: exit %d %q", code, errOut)
	}
	if code, _, _ := s.docker("inspect", c.ID()[:6]); code != 1 {
		t.Errorf("a 6-character prefix must not match")
	}
	if code, _, _ := s.docker("inspect", "--format", "{{.Id}}", c.ContainerID); code != exitShim {
		t.Errorf("--format should be refused")
	}
	if s.execPath != "" {
		t.Errorf("pod mode reached a docker: %q", s.execArgv)
	}
}

func TestPodModeHasNoEngine(t *testing.T) {
	s := newTestShim(t, Config{Mode: ModePod, Pod: &PodConfig{Container: testContainer()}})
	for _, args := range [][]string{{"-v"}, {"version", "--format", "{{.Server.Version}}"}, {"buildx", "version"}} {
		if code, _, errOut := s.docker(args...); code != 1 || !strings.Contains(errOut, "no docker engine") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"ps"}, {"run", "img"}, {"events"}, {"rm", "x"}, {"start", "x"}, {"pull", "img"}} {
		if code, _, _ := s.docker(args...); code != exitShim {
			t.Errorf("%v: exit %d, want refused", args, code)
		}
	}
	if s.execPath != "" {
		t.Errorf("pod mode reached a docker: %q", s.execArgv)
	}
}

// fakeExecer records the execs it is asked for and runs a scripted body.
type fakeExecer struct {
	mu   sync.Mutex
	reqs []ExecRequest
	body func(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error)
}

func (f *fakeExecer) Exec(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return f.body(ctx, req, stdio)
}

// servePod starts ServeExec for s's exec socket.
func servePod(t *testing.T, s *testShim, ex Execer) {
	t.Helper()
	ln, err := net.Listen("unix", s.dir.ExecSocket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeExec(ctx, ln, ex) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ServeExec: %v", err)
		}
	})
}

func TestPodModeExec(t *testing.T) {
	c := testContainer()
	s := newTestShim(t, Config{Mode: ModePod, Pod: &PodConfig{Container: c}})
	ex := &fakeExecer{body: func(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error) {
		var in []byte
		if stdio.Stdin != nil {
			in, _ = io.ReadAll(stdio.Stdin)
		}
		_, _ = stdio.Stdout.Write([]byte("out:" + string(in)))
		_, _ = stdio.Stderr.Write([]byte("err:" + strings.Join(req.Argv, " ")))
		return 7, nil
	}}
	servePod(t, s, ex)
	s.env["FROM_HOST"] = "h"
	if err := os.WriteFile(filepath.Join(s.cwd, "exec.env"), []byte("FILE=f\nFILE_BARE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.call(dockerLink, "hello stdin",
		"exec", "-i", "-u", "vscode", "--env-file", "exec.env", "-e", "A=1", "-e", "FROM_HOST", "-e", "UNSET", "-w", "/workspaces/app",
		c.ID(), "/bin/sh", "-c", "cat")
	if code != 7 {
		t.Fatalf("exit %d, want the command's 7 (stderr %q)", code, errOut)
	}
	if out != "out:hello stdin" || errOut != "err:/bin/sh -c cat" {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
	want := ExecRequest{User: "vscode", Env: []string{"FILE=f", "A=1", "FROM_HOST=h"}, Dir: "/workspaces/app", Argv: []string{"/bin/sh", "-c", "cat"}, Stdin: true}
	if len(ex.reqs) != 1 || !reflect.DeepEqual(ex.reqs[0], want) {
		t.Errorf("request\n got %+v\nwant %+v", ex.reqs, want)
	}

	// Without -i the command gets no stdin.
	ex.reqs = nil
	if code, out, _ := s.call(dockerLink, "ignored", "exec", "-u", "root", c.ContainerID, "true"); code != 7 || out != "out:" {
		t.Errorf("no -i: exit %d stdout %q", code, out)
	}
	if ex.reqs[0].Stdin {
		t.Errorf("no -i but Stdin set")
	}

	if code, _, errOut := s.docker("exec", "other-container", "true"); code != 1 || !strings.Contains(errOut, "No such container: other-container") {
		t.Errorf("another container: exit %d %q", code, errOut)
	}
	for _, args := range [][]string{
		{"exec", "-d", c.ID(), "true"},
		{"exec", "--privileged", c.ID(), "true"},
		{"exec", "--detach-keys", "ctrl-x", c.ID(), "true"},
		{"exec", c.ID()},
		{"exec"},
		{"exec", "--frobnicate", c.ID(), "true"},
	} {
		if code, _, _ := s.docker(args...); code != exitShim {
			t.Errorf("%v: exit %d, want refused", args, code)
		}
	}
}

func TestPodModeExecTransportFailures(t *testing.T) {
	c := testContainer()
	s := newTestShim(t, Config{Mode: ModePod, Pod: &PodConfig{Container: c}})

	// No server: the exec cannot run.
	if code, _, errOut := s.docker("exec", c.ID(), "true"); code != exitShim || !strings.Contains(errOut, "connecting to fleetd") {
		t.Errorf("no server: exit %d %q", code, errOut)
	}

	servePod(t, s, ExecerFunc(func(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error) {
		return 0, errors.New("the pod's agent session dropped")
	}))
	code, _, errOut := s.docker("exec", c.ID(), "true")
	if code != exitShim || !strings.Contains(errOut, "the pod's agent session dropped") {
		t.Errorf("an Execer error: exit %d %q", code, errOut)
	}
}

// TestServeExecCancelsWhenTheShimGoes: an exec whose shim disconnects is
// cancelled, and ServeExec cancels what still runs when it stops.
func TestServeExecCancelsWhenTheShimGoes(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan struct{})
	started := make(chan struct{})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeExec(ctx, ln, ExecerFunc(func(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return 0, ctx.Err()
		}))
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fw := &frameWriter{w: conn}
	payload, _ := json.Marshal(ExecRequest{Argv: []string{"sleep", "100"}, Stdin: true})
	if err := fw.write(frameRequest, payload); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = conn.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the exec was not cancelled when its shim went away")
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("ServeExec: %v", err)
	}
}

// TestServeExecRejectsBadRequests: the first frame must be a request with a
// command.
func TestServeExecRejectsBadRequests(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		_ = ServeExec(ctx, ln, ExecerFunc(func(context.Context, ExecRequest, ExecStdio) (int, error) { return 0, nil }))
	}()
	for _, tc := range []struct {
		typ     byte
		payload []byte
		want    string
	}{
		{frameStdin, []byte("x"), "expected a request first"},
		{frameRequest, []byte("{"), "bad request"},
		{frameRequest, []byte(`{"argv":[]}`), "no command"},
	} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		fw := &frameWriter{w: conn}
		_ = fw.write(tc.typ, tc.payload)
		typ, payload, err := readFrame(conn)
		if err != nil || typ != frameError || !strings.Contains(string(payload), tc.want) {
			t.Errorf("%q: frame %d %q %v", tc.want, typ, payload, err)
		}
		_ = conn.Close()
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	fw := &frameWriter{w: &buf}
	big := bytes.Repeat([]byte("x"), chunkSize*2+5)
	if _, err := (streamWriter{fw: fw, typ: frameStdout}).Write(big); err != nil {
		t.Fatal(err)
	}
	if err := fw.write(frameExit, exitPayload(-1)); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		typ, payload, err := readFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if typ == frameExit {
			code, err := exitCode(payload)
			if err != nil || code != -1 {
				t.Errorf("exit code %d %v", code, err)
			}
			break
		}
		if len(payload) > chunkSize {
			t.Errorf("a %d-byte data frame", len(payload))
		}
		got = append(got, payload...)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("data came back as %d bytes, want %d", len(got), len(big))
	}
	if err := fw.write(frameStdout, make([]byte, maxFrame+1)); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("an oversized frame: %v", err)
	}
	hdr := []byte{frameStdout, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := readFrame(bytes.NewReader(hdr)); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("reading an oversized frame: %v", err)
	}
	if _, _, err := readFrame(bytes.NewReader([]byte{frameStdout, 0, 0, 0, 4, 'a'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a truncated frame: %v", err)
	}
}

// shortTempDir is a temp dir short enough for a unix socket path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kubeshim-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
