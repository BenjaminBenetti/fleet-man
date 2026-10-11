package browserproxy

import (
	"bufio"
	"bytes"
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
)

// startProxy serves the proxy on a loopback port and returns its address.
func startProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(New(nil))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// proxyClient is an HTTP client that sends every request through the proxy.
func proxyClient(proxyAddr string) *http.Client {
	u := &url.URL{Scheme: "http", Host: proxyAddr}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u), DisableCompression: true}, Timeout: 20 * time.Second}
}

// rawRequest writes req to the proxy as is and returns the connection and a
// reader over its answer.
func rawRequest(t *testing.T, proxyAddr, req string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// Headers pass as they came, both ways: several Set-Cookie headers stay
// several, a request header with several values keeps them, nothing adds an
// X-Forwarded-For, and the status and body are the target's.
func TestHeadersPassVerbatim(t *testing.T) {
	var seen http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Add("Set-Cookie", "fleet_a=1; Path=/")
		w.Header().Add("Set-Cookie", "fleet_b=2; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "fleet_c=3; Path=/; Max-Age=600")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "short and stout")
	}))
	defer target.Close()
	client := proxyClient(startProxy(t))

	req, _ := http.NewRequest("GET", target.URL+"/cookies?x=1", nil)
	req.Header.Add("X-Custom", "one")
	req.Header.Add("X-Custom", "two")
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTeapot || string(body) != "short and stout" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	cookies := resp.Header.Values("Set-Cookie")
	want := []string{"fleet_a=1; Path=/", "fleet_b=2; Path=/; HttpOnly", "fleet_c=3; Path=/; Max-Age=600"}
	if strings.Join(cookies, "|") != strings.Join(want, "|") {
		t.Fatalf("Set-Cookie headers %q, want %q", cookies, want)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("response headers %v", resp.Header)
	}
	if got := seen.Values("X-Custom"); strings.Join(got, ",") != "one,two" {
		t.Fatalf("the target saw X-Custom %q", got)
	}
	if seen.Get("Accept-Encoding") != "gzip, br" {
		t.Fatalf("the target saw Accept-Encoding %q", seen.Get("Accept-Encoding"))
	}
	if _, ok := seen["X-Forwarded-For"]; ok {
		t.Fatalf("the proxy added X-Forwarded-For: %v", seen)
	}
	if _, ok := seen["Proxy-Connection"]; ok {
		t.Fatalf("a hop-by-hop header reached the target: %v", seen)
	}
}

// A chunked upload streams through, and an answer streams back before the
// target has finished it.
func TestBodiesStream(t *testing.T) {
	first := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "got %d\n", n)
		w.(http.Flusher).Flush()
		<-first
		fmt.Fprint(w, "done\n")
	}))
	defer target.Close()
	defer close(first)
	client := proxyClient(startProxy(t))

	pr, pw := io.Pipe()
	go func() {
		for range 64 {
			_, _ = pw.Write(bytes.Repeat([]byte("u"), 16<<10))
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest("POST", target.URL+"/upload", pr)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "got "+strconv.Itoa(64*16<<10)+"\n" {
		t.Fatalf("the streamed answer's first line: %q, %v", line, err)
	}
}

// An HTTP/1.0 client is served, and the answer ends with the connection.
func TestHTTP10Client(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "old client ok")
	}))
	defer target.Close()
	_, r := rawRequest(t, startProxy(t), "GET "+target.URL+"/ HTTP/1.0\r\n\r\n")
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "old client ok" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

// CONNECT tunnels to any port: bytes sent right behind the request (a TLS
// hello) arrive, and a half-close passes on, so a late answer still comes.
func TestConnectTunnelsAndHalfCloses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		n, _ := io.Copy(io.Discard, c)
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintf(c, "read %d", n)
	}()
	addr := ln.Addr().String()
	c, r := rawRequest(t, startProxy(t), "CONNECT "+addr+" HTTP/1.1\r\nHost: "+addr+"\r\n\r\nearly bytes")
	resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	if _, err := io.WriteString(c, "+more"); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "read "+strconv.Itoa(len("early bytes+more")) {
		t.Fatalf("through the tunnel: %q, %v", got, err)
	}
}

// wsTarget answers a WebSocket upgrade with 101 and then echoes bytes.
func wsTarget(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade only", http.StatusBadRequest)
			return
		}
		c, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		fmt.Fprint(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n")
		_, _ = io.Copy(c, buf)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func upgradeRequest(target, host string) string {
	return "GET " + target + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
}

func echoAfterUpgrade(t *testing.T, c net.Conn, r *bufio.Reader, req *http.Request) {
	t.Helper()
	resp, err := http.ReadResponse(r, req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("the upgrade: %+v, %v", resp, err)
	}
	if _, err := io.WriteString(c, "ws frame bytes"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("ws frame bytes"))
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "ws frame bytes" {
		t.Fatalf("after the upgrade: %q, %v", buf, err)
	}
}

// A WebSocket inside a CONNECT tunnel, the way a browser opens one through a
// proxy.
func TestWebSocketThroughConnect(t *testing.T) {
	host := strings.TrimPrefix(wsTarget(t).URL, "http://")
	c, r := rawRequest(t, startProxy(t), "CONNECT "+host+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	if _, err := io.WriteString(c, upgradeRequest("/ws", host)); err != nil {
		t.Fatal(err)
	}
	echoAfterUpgrade(t, c, r, &http.Request{Method: "GET"})
}

// A WebSocket upgrade sent to the proxy as an absolute-form request is
// relayed too.
func TestWebSocketThroughAPlainRequest(t *testing.T) {
	target := wsTarget(t)
	host := strings.TrimPrefix(target.URL, "http://")
	c, r := rawRequest(t, startProxy(t), upgradeRequest(target.URL+"/ws", host))
	echoAfterUpgrade(t, c, r, &http.Request{Method: "GET"})
}

func TestRefusals(t *testing.T) {
	proxyAddr := startProxy(t)
	_, r := rawRequest(t, proxyAddr, "GET /not-proxied HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp, err := http.ReadResponse(r, nil); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an origin-form request: %+v, %v", resp, err)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	_ = ln.Close()
	resp, err := proxyClient(proxyAddr).Get("http://" + closed + "/")
	if err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a closed port: %+v, %v", resp, err)
	}
	_ = resp.Body.Close()
	_, r = rawRequest(t, proxyAddr, "CONNECT "+closed+" HTTP/1.1\r\nHost: "+closed+"\r\n\r\n")
	if resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"}); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT to a closed port: %+v, %v", resp, err)
	}
}

func TestRunListensAndStops(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	w := &syncBuffer{b: &logs}
	go func() { done <- Run(ctx, Config{Addr: "127.0.0.1", Port: port, Log: w}) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.String(), "listening on 127.0.0.1:"+strconv.Itoa(port)) {
		if time.Now().After(deadline) {
			t.Fatalf("no listening line: %q", w.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

// syncBuffer is a bytes.Buffer safe to write and read from two goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
