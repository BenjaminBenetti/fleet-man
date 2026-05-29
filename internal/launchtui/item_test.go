package launchtui

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
)

// TestBuildItems verifies links are flattened before apps, fields are carried
// across, and linkCount reports the boundary.
func TestBuildItems(t *testing.T) {
	fl := devcontainer.FleetCustomizations{
		FleetLaunch: devcontainer.FleetLaunchCustomizations{
			Sites: []devcontainer.FleetLaunchSite{
				{Title: "Docs", SubTitle: "reference", URL: "https://docs"},
			},
			Apps: []devcontainer.FleetLaunchApp{
				{Title: "Web", Command: "npm start", Port: 3000},
			},
		},
	}
	items := buildItems(fl)
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].kind != kindLink || items[0].url != "https://docs" || items[0].subtitle != "reference" {
		t.Errorf("link item wrong: %+v", items[0])
	}
	if items[1].kind != kindApp || items[1].command != "npm start" || items[1].port != 3000 {
		t.Errorf("app item wrong: %+v", items[1])
	}
	if items[1].subtitle != "localhost:3000" {
		t.Errorf("app subtitle = %q, want localhost:3000", items[1].subtitle)
	}
	if got := linkCount(items); got != 1 {
		t.Errorf("linkCount = %d, want 1", got)
	}
}

// fakeClient records OpenBrowser calls and can be made to fail, standing in for
// a *control.Client in model tests.
type fakeClient struct {
	urls []string
	err  error
}

func (f *fakeClient) OpenBrowser(url string) error {
	f.urls = append(f.urls, url)
	return f.err
}

// TestActivateLink confirms activating a link sets an "Opening…" status and
// returns an async command (it must NOT call OpenBrowser inline, so a slow or
// hung host can't block the UI thread); running that command then sends the
// link's URL to the client.
func TestActivateLink(t *testing.T) {
	items := []item{{kind: kindLink, title: "Docs", url: "https://docs"}}
	fc := &fakeClient{}
	m := model{items: items, links: 1, client: fc}

	next, cmd := m.activate(0)
	nm := next.(model)
	if cmd == nil {
		t.Fatalf("expected a tea.Cmd for link activation")
	}
	// OpenBrowser must not have been called synchronously on the UI thread.
	if len(fc.urls) != 0 {
		t.Errorf("OpenBrowser called inline for link; want async only")
	}
	if nm.status == "" {
		t.Errorf("expected a non-empty status after activating a link")
	}

	// Running the async command sends the link's URL.
	msg := cmd()
	res, ok := msg.(appOpenedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want appOpenedMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("err = %v, want nil", res.err)
	}
	if len(fc.urls) != 1 || fc.urls[0] != "https://docs" {
		t.Fatalf("OpenBrowser calls = %v, want [https://docs]", fc.urls)
	}
}

// TestActivateDegraded confirms that with no client, activation sets an error
// status and does not panic.
func TestActivateDegraded(t *testing.T) {
	items := []item{{kind: kindLink, title: "Docs", url: "https://docs"}}
	m := model{items: items, links: 1} // client nil

	next, cmd := m.activate(0)
	nm := next.(model)
	if cmd != nil {
		t.Errorf("expected no command when degraded")
	}
	if nm.status == "" {
		t.Errorf("expected an error status when no client")
	}
}

// TestActivateAppReturnsCmd confirms an app activation returns a command (the
// async start) rather than calling OpenBrowser inline.
func TestActivateAppReturnsCmd(t *testing.T) {
	items := []item{{kind: kindApp, title: "Web", command: "true", port: 65535}}
	fc := &fakeClient{}
	m := model{items: items, links: 0, client: fc}

	_, cmd := m.activate(0)
	if cmd == nil {
		t.Fatalf("expected a tea.Cmd for app activation")
	}
	// OpenBrowser must not have been called synchronously.
	if len(fc.urls) != 0 {
		t.Errorf("OpenBrowser called inline for app; want async only")
	}
}

// TestOpenAppCmdAlreadyUp drives the async app command against a port that is
// already answering (an httptest server): EnsureRunningOnPort returns nil
// immediately, so the command should open localhost:<port> on the client and
// report success with no error. This avoids the 15s start deadline by ensuring
// the port is already reachable.
func TestOpenAppCmdAlreadyUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	port := srv.Listener.Addr().(*net.TCPAddr).Port
	fc := &fakeClient{}
	it := item{kind: kindApp, title: "Web", command: "", port: port}

	msg := openAppCmd(fc, it)()
	res, ok := msg.(appOpenedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want appOpenedMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("err = %v, want nil for an already-up port", res.err)
	}
	wantURL := "http://localhost:" + strconv.Itoa(port)
	if len(fc.urls) != 1 || fc.urls[0] != wantURL {
		t.Errorf("OpenBrowser calls = %v, want [%s]", fc.urls, wantURL)
	}
}
