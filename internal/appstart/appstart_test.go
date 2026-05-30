package appstart

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReachable verifies Reachable is true against a live server (even one
// returning an error status) and false against a stopped one.
func TestReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 500 is still "reachable": the server answered.
		w.WriteHeader(http.StatusInternalServerError)
	}))

	if !Reachable(srv.URL) {
		t.Errorf("Reachable(%q) = false, want true for a live server", srv.URL)
	}

	srv.Close()
	if Reachable(srv.URL) {
		t.Errorf("Reachable(%q) = true after close, want false", srv.URL)
	}
}

// TestWaitForReachable verifies WaitForReachable returns true for a live
// server and false (after the timeout) for an address with nothing listening.
func TestWaitForReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !WaitForReachable(srv.URL, time.Second) {
		t.Errorf("WaitForReachable(live) = false, want true")
	}

	// A port nothing listens on must time out to false. Use a short timeout so
	// the test stays fast.
	if WaitForReachable("http://127.0.0.1:0", 400*time.Millisecond) {
		t.Errorf("WaitForReachable(dead) = true, want false")
	}
}

// TestLocalURL verifies the conventional localhost URL formatting.
func TestLocalURL(t *testing.T) {
	cases := []struct {
		port int
		want string
	}{
		{3000, "http://localhost:3000"},
		{16767, "http://localhost:16767"},
	}
	for _, tc := range cases {
		if got := LocalURL(tc.port); got != tc.want {
			t.Errorf("LocalURL(%d) = %q, want %q", tc.port, got, tc.want)
		}
	}
}

// TestEnsureRunningOnPortAlreadyUp verifies an empty command against an
// already-listening port returns nil immediately without starting anything.
func TestEnsureRunningOnPortAlreadyUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := serverPort(t, srv)
	if err := EnsureRunningOnPort("", port); err != nil {
		t.Errorf("EnsureRunningOnPort(\"\", %d) = %v, want nil for an already-up port", port, err)
	}
}

// serverPort extracts the numeric port an httptest.Server listens on.
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	// httptest binds 127.0.0.1:<port>; pull the port from the TCPAddr.
	if tcpAddr, ok := srv.Listener.Addr().(*net.TCPAddr); ok {
		return tcpAddr.Port
	}
	t.Fatalf("could not determine server port from %v", srv.Listener.Addr())
	return 0
}
