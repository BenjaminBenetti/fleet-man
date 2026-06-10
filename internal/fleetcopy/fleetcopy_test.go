package fleetcopy

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// TestRequestSendsEnvelope round-trips the in-instance form against a fake
// host listener: the sent envelope is a file.copy naming the absolute path.
func TestRequestSendsEnvelope(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tool")
	if err := os.WriteFile(file, []byte("bytes"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	socket := filepath.Join(dir, "fleet.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	got := make(chan control.Envelope, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var env control.Envelope
		_ = json.NewDecoder(conn).Decode(&env)
		got <- env
	}()

	var out bytes.Buffer
	if err := Request(Config{SocketPath: socket}, &out, file, "~/builds/tool"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	env := <-got
	if env.Type != control.TypeCopyFile {
		t.Fatalf("envelope type = %q, want %q", env.Type, control.TypeCopyFile)
	}
	var payload control.CopyFilePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Path != file {
		t.Fatalf("payload path = %q, want %q", payload.Path, file)
	}
	if payload.Dest != "~/builds/tool" {
		t.Fatalf("payload dest = %q, want ~/builds/tool (passed through verbatim)", payload.Dest)
	}
	if !strings.Contains(out.String(), file) || !strings.Contains(out.String(), "~/builds/tool") {
		t.Fatalf("confirmation %q does not name the file and destination", out.String())
	}
}

// TestRequestErrors covers the fast local failures: a missing file, a
// directory, and no host listener.
func TestRequestErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SocketPath: filepath.Join(dir, "absent.sock")}

	var out bytes.Buffer
	if err := Request(cfg, &out, filepath.Join(dir, "ghost"), ""); !os.IsNotExist(err) {
		t.Fatalf("missing file: want IsNotExist, got %v", err)
	}
	if err := Request(cfg, &out, dir, ""); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory: want directory error, got %v", err)
	}

	file := filepath.Join(dir, "tool")
	if err := os.WriteFile(file, []byte("bytes"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := Request(cfg, &out, file, ""); err == nil || !strings.Contains(err.Error(), "not connected to a host fleet") {
		t.Fatalf("no listener: want not-connected error, got %v", err)
	}
}
