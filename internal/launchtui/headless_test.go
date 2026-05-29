package launchtui

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// writeSampleConfig writes a devcontainer.json with a fleetLaunch block (two
// links, two apps) to a temp file and returns its path.
func writeSampleConfig(t *testing.T) string {
	t.Helper()
	const cfg = `{
  "customizations": {
    "fleet": {
      "fleetLaunch": {
        "sites": [
          { "title": "API", "url": "http://localhost:3000" },
          { "title": "Docs", "url": "http://localhost:8080" }
        ],
        "apps": [
          { "title": "Grafana", "port": 3000, "command": "echo grafana" },
          { "title": "Logs", "port": 16768 }
        ]
      }
    }
  }
}`
	path := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestResolveItem checks case-insensitive exact and unique-prefix matching,
// exact-wins-over-prefix, ambiguity, and the not-found case.
func TestResolveItem(t *testing.T) {
	// indices: 0 API, 1 Docs, 2 Admin (links), 3 Grafana (app)
	items, _ := mkItems([]string{"API", "Docs", "Admin"}, []string{"Grafana"})

	resolved := []struct {
		name string
		want int
	}{
		{"API", 0},  // exact
		{"api", 0},  // exact, case-insensitive
		{"docs", 1}, // exact
		{"graf", 3}, // unique prefix
		{"d", 1},    // unique prefix (only Docs starts with d)
	}
	for _, c := range resolved {
		if got, m := resolveItem(items, c.name); got != c.want || m != nil {
			t.Errorf("resolveItem(%q) = (%d, %v), want (%d, nil)", c.name, got, m, c.want)
		}
	}

	// Ambiguous prefix: both API and Admin start with "a"; returned in item order.
	if got, m := resolveItem(items, "a"); got != -1 || len(m) != 2 || m[0] != "API" || m[1] != "Admin" {
		t.Errorf("resolveItem(%q) = (%d, %v), want (-1, [API Admin])", "a", got, m)
	}

	// No match.
	if got, m := resolveItem(items, "zzz"); got != -1 || len(m) != 0 {
		t.Errorf("resolveItem(%q) = (%d, %v), want (-1, [])", "zzz", got, m)
	}

	// An exact match wins even when a longer sibling shares the prefix.
	items2, _ := mkItems([]string{"Log", "Logs"}, nil)
	if got, m := resolveItem(items2, "log"); got != 0 || m != nil {
		t.Errorf("resolveItem(%q) = (%d, %v), want (0, nil) — exact should win", "log", got, m)
	}
}

// TestList prints both sections with their entries and targets.
func TestList(t *testing.T) {
	var buf bytes.Buffer
	if err := List(Config{ConfigPath: writeSampleConfig(t)}, &buf); err != nil {
		t.Fatalf("List: %v", err)
	}
	out := buf.String()

	// Sections present and ordered Links before Apps.
	li := strings.Index(out, "Links:")
	ai := strings.Index(out, "Apps:")
	if li < 0 || ai < 0 || li > ai {
		t.Fatalf("expected Links: before Apps: in output:\n%s", out)
	}
	for _, want := range []string{"API", "http://localhost:3000", "Docs", "Grafana", "Logs", "localhost:16768"} {
		if !strings.Contains(out, want) {
			t.Errorf("List output missing %q:\n%s", want, out)
		}
	}
}

// TestListEmptySections shows "(none)" under a section with no entries.
func TestListEmptySections(t *testing.T) {
	const cfg = `{"customizations":{"fleet":{"fleetLaunch":{
      "sites":[{"title":"Only","url":"http://x"}]}}}}`
	path := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := List(Config{ConfigPath: path}, &buf); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(buf.String(), "(none)") {
		t.Errorf("expected an empty Apps section to render (none):\n%s", buf.String())
	}
}

// TestLaunchByNameLink dials a real control server and asserts launching a link
// by name sends an open-browser request for that link's URL.
func TestLaunchByNameLink(t *testing.T) {
	got := make(chan control.Envelope, 1)
	sock := filepath.Join(t.TempDir(), control.SocketName)
	srv, err := control.Listen(sock, func(e control.Envelope) { got <- e })
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cfg := Config{ConfigPath: writeSampleConfig(t), SocketPath: sock}
	var out bytes.Buffer
	if err := LaunchByName(cfg, "docs", &out); err != nil { // case-insensitive
		t.Fatalf("LaunchByName: %v", err)
	}
	if !strings.Contains(out.String(), "Opened Docs") {
		t.Errorf("expected an 'Opened Docs' confirmation, got %q", out.String())
	}

	select {
	case e := <-got:
		if e.Type != control.TypeOpenBrowser {
			t.Fatalf("envelope type = %q, want %q", e.Type, control.TypeOpenBrowser)
		}
		if !strings.Contains(string(e.Payload), "http://localhost:8080") {
			t.Errorf("payload %q does not carry the Docs URL", string(e.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the open-browser request")
	}
}

// TestLaunchByNamePrefix launches by a unique leading fragment of a name: "ap"
// resolves to "API" and its URL is sent to the host.
func TestLaunchByNamePrefix(t *testing.T) {
	got := make(chan control.Envelope, 1)
	sock := filepath.Join(t.TempDir(), control.SocketName)
	srv, err := control.Listen(sock, func(e control.Envelope) { got <- e })
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cfg := Config{ConfigPath: writeSampleConfig(t), SocketPath: sock}
	if err := LaunchByName(cfg, "ap", io.Discard); err != nil { // unique prefix of "API"
		t.Fatalf("LaunchByName(prefix): %v", err)
	}
	select {
	case e := <-got:
		if !strings.Contains(string(e.Payload), "http://localhost:3000") {
			t.Errorf("payload %q does not carry the API URL", string(e.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the open-browser request")
	}
}

// TestLaunchByNameAmbiguous reports every candidate when a prefix matches more
// than one item, and does so without needing a host connection.
func TestLaunchByNameAmbiguous(t *testing.T) {
	const cfg = `{"customizations":{"fleet":{"fleetLaunch":{"sites":[
      {"title":"Grafana","url":"http://a"},{"title":"Graphite","url":"http://b"}]}}}}`
	path := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	err := LaunchByName(Config{ConfigPath: path}, "gra", io.Discard)
	if err == nil {
		t.Fatal("LaunchByName(ambiguous) = nil, want error")
	}
	if !strings.Contains(err.Error(), "Grafana") || !strings.Contains(err.Error(), "Graphite") {
		t.Errorf("ambiguous error should list both candidates: %v", err)
	}
}

// TestLaunchByNameNotFound returns a helpful error for an unknown name without
// needing a host connection.
func TestLaunchByNameNotFound(t *testing.T) {
	err := LaunchByName(Config{ConfigPath: writeSampleConfig(t)}, "ghost", io.Discard)
	if err == nil {
		t.Fatal("LaunchByName(unknown) = nil, want error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q should name the missing item", err)
	}
}

// TestLaunchByNameNoHost errors clearly when the control socket can't be dialled
// (no host listening).
func TestLaunchByNameNoHost(t *testing.T) {
	cfg := Config{
		ConfigPath: writeSampleConfig(t),
		SocketPath: filepath.Join(t.TempDir(), "absent.sock"),
	}
	err := LaunchByName(cfg, "API", io.Discard)
	if err == nil {
		t.Fatal("LaunchByName with no host = nil, want error")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error %q should mention the missing host", err)
	}
}

// TestLaunchByNameAppProgress launches an app whose port is already answering
// (so the boot wait returns immediately) and asserts the user gets progress
// feedback: a "Starting <app>…" line and an "Opened <app>" confirmation, and
// the host receives the localhost URL. The progress writer is a plain buffer
// (not a TTY), so the spinner degrades to a single static line — keeping the
// assertion deterministic.
func TestLaunchByNameAppProgress(t *testing.T) {
	// A live HTTP server stands in for the already-running app on its port.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer app.Close()
	u, err := url.Parse(app.URL)
	if err != nil {
		t.Fatalf("parse httptest URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	// Control server to receive the open-browser request.
	gotMsg := make(chan control.Envelope, 1)
	sock := filepath.Join(t.TempDir(), control.SocketName)
	srv, err := control.Listen(sock, func(e control.Envelope) { gotMsg <- e })
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	// Config: one app, no command (rely on the already-listening port), pointed
	// at the httptest server's port.
	cfgJSON := fmt.Sprintf(`{"customizations":{"fleet":{"fleetLaunch":{
      "apps":[{"title":"Metrics","port":%d}]}}}}`, port)
	path := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := LaunchByName(Config{ConfigPath: path, SocketPath: sock}, "metrics", &out); err != nil {
		t.Fatalf("LaunchByName(app): %v", err)
	}

	feedback := out.String()
	if !strings.Contains(feedback, "Starting Metrics") {
		t.Errorf("expected a 'Starting Metrics' progress line, got %q", feedback)
	}
	if !strings.Contains(feedback, "Opened Metrics") {
		t.Errorf("expected an 'Opened Metrics' confirmation, got %q", feedback)
	}

	select {
	case e := <-gotMsg:
		want := fmt.Sprintf("http://localhost:%d", port)
		if !strings.Contains(string(e.Payload), want) {
			t.Errorf("payload %q does not carry %q", string(e.Payload), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the open-browser request")
	}
}
