package fleetcopy

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// TestRequestSendsEnvelope round-trips the in-instance form against a fake host
// listener: the sent envelope is a file.copy naming the two endpoints verbatim,
// as typed inside the instance (no local resolution — it is a pure signal).
func TestRequestSendsEnvelope(t *testing.T) {
	dir := t.TempDir()
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
	if err := Request(Config{SocketPath: socket}, &out, ":build/tool", "~/builds/tool", false); err != nil {
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
	if payload.Src != ":build/tool" {
		t.Fatalf("payload src = %q, want :build/tool (verbatim)", payload.Src)
	}
	if payload.Dst != "~/builds/tool" {
		t.Fatalf("payload dst = %q, want ~/builds/tool (verbatim)", payload.Dst)
	}
	if !strings.Contains(out.String(), ":build/tool") || !strings.Contains(out.String(), "~/builds/tool") {
		t.Fatalf("confirmation %q does not name the endpoints", out.String())
	}
}

// TestRequestDownloadShorthand confirms the 1-arg form sends an empty dst and
// the confirmation mentions the downloads folder.
func TestRequestDownloadShorthand(t *testing.T) {
	dir := t.TempDir()
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
	if err := Request(Config{SocketPath: socket}, &out, ":out.bin", "", false); err != nil {
		t.Fatalf("Request: %v", err)
	}
	var payload control.CopyFilePayload
	if err := json.Unmarshal((<-got).Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Src != ":out.bin" || payload.Dst != "" {
		t.Fatalf("payload = %+v, want src=:out.bin dst empty", payload)
	}
	if !strings.Contains(out.String(), "downloads") {
		t.Fatalf("confirmation %q does not mention downloads", out.String())
	}
}

// TestRequestOpen confirms the `fleet open` form sets the open flag on the same
// file.copy envelope and says so in the confirmation.
func TestRequestOpen(t *testing.T) {
	dir := t.TempDir()
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
	if err := Request(Config{SocketPath: socket}, &out, ":chart.png", "", true); err != nil {
		t.Fatalf("Request: %v", err)
	}
	env := <-got
	if env.Type != control.TypeCopyFile {
		t.Fatalf("envelope type = %q, want %q (open rides the copy envelope)", env.Type, control.TypeCopyFile)
	}
	var payload control.CopyFilePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Src != ":chart.png" || payload.Dst != "" || !payload.Open {
		t.Fatalf("payload = %+v, want src=:chart.png dst empty open=true", payload)
	}
	if !strings.Contains(out.String(), "opening") {
		t.Fatalf("confirmation %q does not mention opening", out.String())
	}
}

// TestRequestNoListener covers the only failure Request still reports locally:
// no host TUI connected. It no longer stats the source (which may be a file on
// the user's machine, unreachable from in-container) — that is the host TUI's job.
func TestRequestNoListener(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SocketPath: filepath.Join(dir, "absent.sock")}

	var out bytes.Buffer
	if err := Request(cfg, &out, ":tool", "", false); err == nil || !strings.Contains(err.Error(), "not connected to a host fleet") {
		t.Fatalf("no listener: want not-connected error, got %v", err)
	}
}
