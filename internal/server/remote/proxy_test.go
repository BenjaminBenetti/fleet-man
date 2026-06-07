package remote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// tcpPair returns a connected pair of loopback TCP conns. Used instead of
// net.Pipe for the proxy stress test because it has real socket buffering, which
// better matches production and avoids net.Pipe's strict-synchrony quirks under
// many concurrent multiplexed streams.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return client, got.c
}

// gatewayHTTPClient builds an http.Client whose transport dials a fresh yamux
// stream per request (DisableKeepAlives), exactly as the real gateway's reverse
// proxy will. The request URL host is irrelevant — the dialer ignores it.
func gatewayHTTPClient(session interface {
	Open() (net.Conn, error)
}) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return session.Open()
			},
		},
	}
}

// fakeMCP is a stand-in for the loopback MCP server. /sse streams numbered SSE
// events (with a per-event delay) and records the Authorization header it saw,
// keyed by the request's id; /echo returns the Authorization header in the body.
type fakeMCP struct {
	srv      *httptest.Server
	mu       sync.Mutex
	authByID map[string]string
}

func newFakeMCP() *fakeMCP {
	f := &fakeMCP{authByID: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		delayMS, _ := strconv.Atoi(r.URL.Query().Get("delay"))
		f.mu.Lock()
		f.authByID[id] = r.Header.Get("Authorization")
		f.mu.Unlock()
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: %s-%d\n\n", id, i)
			flusher.Flush()
			if delayMS > 0 {
				time.Sleep(time.Duration(delayMS) * time.Millisecond)
			}
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeMCP) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatalf("parse fake mcp url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return p
}

func (f *fakeMCP) authFor(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authByID[id]
}

// readSSEData reads "data: X" payloads from an SSE body in order.
func readSSEData(t *testing.T, body io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

// TestServeProxyConcurrentSSEAndAuth is the load-bearing transport test: many
// SSE streams driven concurrently over ONE yamux session must each deliver their
// events complete and IN ORDER (no cross-stream interleaving / corruption), and
// the per-request Authorization header must reach the loopback server untouched.
func TestServeProxyConcurrentSSEAndAuth(t *testing.T) {
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	fleetdConn, gatewayConn := tcpPair(t)
	fleetdSession, err := tunnel.ClientSession(fleetdConn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	defer fleetdSession.Close()
	gatewaySession, err := tunnel.ServerSession(gatewayConn, io.Discard)
	if err != nil {
		t.Fatalf("server session: %v", err)
	}
	defer gatewaySession.Close()

	go func() { _ = serveProxy(fleetdSession, mcp.port(t)) }()

	client := gatewayHTTPClient(gatewaySession)

	const (
		streams        = 24
		eventsPerStream = 8
	)
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for g := 0; g < streams; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := fmt.Sprintf("s%d", g)
			auth := fmt.Sprintf("Bearer token-%d", g)
			req, _ := http.NewRequest(http.MethodGet,
				fmt.Sprintf("http://tunnel/sse?id=%s&n=%d", id, eventsPerStream), nil)
			req.Header.Set("Authorization", auth)
			resp, err := client.Do(req)
			if err != nil {
				errs <- fmt.Errorf("%s: do: %w", id, err)
				return
			}
			defer resp.Body.Close()
			got := readSSEData(t, resp.Body)
			if len(got) != eventsPerStream {
				errs <- fmt.Errorf("%s: got %d events, want %d", id, len(got), eventsPerStream)
				return
			}
			for i, ev := range got {
				want := fmt.Sprintf("%s-%d", id, i)
				if ev != want {
					errs <- fmt.Errorf("%s: event %d = %q, want %q (interleave/order bug)", id, i, ev, want)
					return
				}
			}
			if seen := mcp.authFor(id); seen != auth {
				errs <- fmt.Errorf("%s: auth = %q, want %q", id, seen, auth)
				return
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestServeProxyStreamsSSEIncrementally proves the proxy does NOT buffer the
// whole response: with a per-event server delay, the first event must reach the
// client well before the handler could have produced the last one.
func TestServeProxyStreamsSSEIncrementally(t *testing.T) {
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	fleetdConn, gatewayConn := tcpPair(t)
	fleetdSession, err := tunnel.ClientSession(fleetdConn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	defer fleetdSession.Close()
	gatewaySession, err := tunnel.ServerSession(gatewayConn, io.Discard)
	if err != nil {
		t.Fatalf("server session: %v", err)
	}
	defer gatewaySession.Close()

	go func() { _ = serveProxy(fleetdSession, mcp.port(t)) }()
	client := gatewayHTTPClient(gatewaySession)

	const (
		n       = 10
		delayMS = 40 // total handler time ~= 400ms
	)
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://tunnel/sse?id=stream&n=%d&delay=%d", n, delayMS), nil)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var firstAt time.Duration
	count := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			if count == 0 {
				firstAt = time.Since(start)
			}
			count++
		}
	}
	if count != n {
		t.Fatalf("got %d events, want %d", count, n)
	}
	// If the proxy buffered the whole body, the first event would only arrive
	// after the handler finished (~n*delay). It must arrive much sooner.
	if firstAt > time.Duration(n*delayMS/2)*time.Millisecond {
		t.Fatalf("first SSE event took %v — looks buffered, not streamed", firstAt)
	}
}

// TestServeProxyForwardsAuthOnUnaryRequest checks a plain (non-SSE) request also
// carries the Authorization header through.
func TestServeProxyForwardsAuthOnUnaryRequest(t *testing.T) {
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	fleetdConn, gatewayConn := tcpPair(t)
	fleetdSession, _ := tunnel.ClientSession(fleetdConn, io.Discard)
	defer fleetdSession.Close()
	gatewaySession, _ := tunnel.ServerSession(gatewayConn, io.Discard)
	defer gatewaySession.Close()
	go func() { _ = serveProxy(fleetdSession, mcp.port(t)) }()
	client := gatewayHTTPClient(gatewaySession)

	req, _ := http.NewRequest(http.MethodGet, "http://tunnel/echo", nil)
	req.Header.Set("Authorization", "Bearer secret-xyz")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "Bearer secret-xyz" {
		t.Fatalf("auth not forwarded: got %q", b)
	}
}
