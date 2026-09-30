package mcpbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// socketPath returns a unix socket path short enough for sun_path.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, SocketName)
}

// fakeDaemon accepts connections on a unix socket and hands each received
// line to handle, which may reply through the connection.
type fakeDaemon struct {
	t    *testing.T
	path string
	ln   net.Listener

	mu    sync.Mutex
	conns []net.Conn
	lines [][]string // per connection, in order
}

func startFakeDaemon(t *testing.T, path string, handle func(d *fakeDaemon, conn net.Conn, index int, line string)) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{t: t, path: path, ln: ln}
	t.Cleanup(d.stop)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			index := len(d.conns)
			d.conns = append(d.conns, conn)
			d.lines = append(d.lines, nil)
			d.mu.Unlock()
			go func() {
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					d.mu.Lock()
					d.lines[index] = append(d.lines[index], line)
					d.mu.Unlock()
					handle(d, conn, index, line)
				}
			}()
		}
	}()
	return d
}

func (d *fakeDaemon) stop() {
	_ = d.ln.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
}

func (d *fakeDaemon) received(index int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if index >= len(d.lines) {
		return nil
	}
	return append([]string(nil), d.lines[index]...)
}

// reply answers a request line with an empty result.
func reply(conn net.Conn, line string) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal([]byte(line), &msg) != nil || msg.Method == "" || len(msg.ID) == 0 {
		return
	}
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{"method":"` + msg.Method + `"}}` + "\n"))
}

// client drives a Bridge through pipes, like an MCP client over stdio.
type client struct {
	t     *testing.T
	in    *io.PipeWriter
	out   *bufio.Reader
	done  chan error
	close func()
}

func startBridge(t *testing.T, path string) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := &Bridge{Dial: Dialer(path), In: inR, Out: outW, timing: testTimings}
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		c.done <- b.Run(ctx)
		_ = outW.Close()
	}()
	c.close = func() {
		cancel()
		_ = inW.Close()
		_ = outR.Close()
		<-exited
	}
	t.Cleanup(c.close)
	return c
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := c.in.Write([]byte(line + "\n")); err != nil {
		c.t.Fatal(err)
	}
}

// next reads the next message the bridge writes to the client.
func (c *client) next() map[string]any {
	c.t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.out.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("reading from the bridge: %v", r.err)
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(r.line), &msg); err != nil {
			c.t.Fatalf("bridge wrote %q: %v", r.line, err)
		}
		return msg
	case <-time.After(10 * time.Second):
		c.t.Fatal("no message from the bridge")
		return nil
	}
}

const (
	initializeLine  = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	initializedLine = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
)

// testTimings keeps the tests quick.
var testTimings = timings{connect: 500 * time.Millisecond, replay: 2 * time.Second, drain: 2 * time.Second, dialRetry: 20 * time.Millisecond, failFast: 2 * time.Second}

func TestBridgeRelays(t *testing.T) {
	path := socketPath(t)
	startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, line string) { reply(conn, line) })
	c := startBridge(t, path)

	c.send(initializeLine)
	if got := c.next(); got["id"] != float64(0) {
		t.Fatalf("initialize reply = %v", got)
	}
	c.send(initializedLine)
	c.send(`{"jsonrpc":"2.0","id":"a-1","method":"tools/list"}`)
	if got := c.next(); got["id"] != "a-1" {
		t.Fatalf("tools/list reply = %v", got)
	}
}

func TestBridgeAnswersInFlightRequestsWhenTheDaemonDrops(t *testing.T) {
	path := socketPath(t)
	startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, line string) {
		if strings.Contains(line, `"tools/call"`) {
			_ = conn.Close() // the daemon goes away mid-call
			return
		}
		reply(conn, line)
	})
	c := startBridge(t, path)
	c.send(initializeLine)
	c.next()
	c.send(initializedLine)
	c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x"}}`)
	got := c.next()
	if got["id"] != float64(7) || got["error"] == nil {
		t.Fatalf("an in-flight request must be answered with an error, got %v", got)
	}
}

// TestBridgeReportsARefusal: a daemon that hangs up without ever answering
// (its peer check refused this process) is not reported as a restart.
func TestBridgeReportsARefusal(t *testing.T) {
	path := socketPath(t)
	startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, _ string) { _ = conn.Close() })
	c := startBridge(t, path)
	c.send(initializeLine)
	got := c.next()
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || !strings.Contains(errObj["message"].(string), "without answering") {
		t.Fatalf("got %v, want the refusal explained", got)
	}
}

func TestBridgeReplaysTheHandshakeOnReconnect(t *testing.T) {
	path := socketPath(t)
	d := startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, line string) { reply(conn, line) })
	c := startBridge(t, path)
	c.send(initializeLine)
	c.next()
	c.send(initializedLine)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	c.next()

	// The daemon restarts.
	d.stop()
	_ = os.Remove(path)
	d2 := startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, line string) { reply(conn, line) })

	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := c.next(); got["id"] != float64(2) || got["result"] == nil {
		t.Fatalf("after the restart the call must go through, got %v", got)
	}
	lines := d2.received(0)
	if len(lines) != 3 {
		t.Fatalf("new session got %d lines, want initialize + initialized + the call: %q", len(lines), lines)
	}
	var replayed struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &replayed); err != nil || replayed.Method != "initialize" ||
		!strings.HasPrefix(replayed.ID, "fleet-bridge-replay-") || !strings.Contains(string(replayed.Params), `"clientInfo"`) {
		t.Fatalf("first line on the new session should replay the client's initialize under the bridge's id: %q", lines[0])
	}
	if lines[1] != initializedLine || !strings.Contains(lines[2], `"id":2`) {
		t.Fatalf("then initialized, then the call: %q", lines[1:])
	}

	// The replayed initialize's reply never reaches the client: the next
	// message it sees answers its own request.
	c.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if got := c.next(); got["id"] != float64(3) {
		t.Fatalf("the client saw %v, want the reply to its ping", got)
	}
}

func TestBridgeReportsAnUnreachableDaemon(t *testing.T) {
	c := startBridge(t, socketPath(t)) // nothing listens there
	c.send(initializeLine)
	got := c.next()
	errObj, _ := got["error"].(map[string]any)
	if got["id"] != float64(0) || errObj == nil || !strings.Contains(errObj["message"].(string), "unreachable") {
		t.Fatalf("got %v, want an 'unreachable' error for the initialize", got)
	}
}

// TestBridgeFailsFastDuringAnOutage: once a connect has given up, requests
// queued behind it are answered at once rather than one connect apart.
func TestBridgeFailsFastDuringAnOutage(t *testing.T) {
	c := startBridge(t, socketPath(t)) // nothing listens there
	c.send(initializeLine)
	c.next() // waited out the connect
	start := time.Now()
	for id := 1; id <= 3; id++ {
		c.send(`{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"tools/list"}`)
		if got := c.next(); got["id"] != float64(id) || got["error"] == nil {
			t.Fatalf("request %d: got %v, want an error", id, got)
		}
	}
	if elapsed := time.Since(start); elapsed > testTimings.connect {
		t.Fatalf("queued requests took %v to fail, want them answered without another connect", elapsed)
	}
}

func TestBridgeDrainsRepliesAfterTheClientCloses(t *testing.T) {
	path := socketPath(t)
	startFakeDaemon(t, path, func(_ *fakeDaemon, conn net.Conn, _ int, line string) {
		time.Sleep(200 * time.Millisecond) // a slow answer
		reply(conn, line)
	})
	c := startBridge(t, path)
	c.send(initializeLine)
	_ = c.in.Close() // the client is done writing
	if got := c.next(); got["id"] != float64(0) {
		t.Fatalf("the reply owed to the client was lost: %v", got)
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not exit once the replies were delivered")
	}
}

// TestBridgeWithGoSDK drives a real MCP client through the bridge to a real
// MCP server, across a server restart.
func TestBridgeWithGoSDK(t *testing.T) {
	path := socketPath(t)

	type echoIn struct {
		Text string `json:"text"`
	}
	type echoOut struct {
		Text string `json:"text"`
	}
	newServer := func() *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: "fleet", Version: "t"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut(in), nil
		})
		return srv
	}
	serve := func() (stop func()) {
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := newServer()
		var mu sync.Mutex
		var conns []net.Conn
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
				go func() { _ = srv.Run(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}) }()
			}
		}()
		return func() {
			cancel()
			_ = ln.Close()
			mu.Lock()
			for _, c := range conns {
				_ = c.Close()
			}
			mu.Unlock()
			_ = os.Remove(path)
		}
	}
	stop := serve()
	t.Cleanup(func() { stop() })

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := &Bridge{Dial: Dialer(path), In: inR, Out: outW, timing: testTimings}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		<-exited
	})

	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "t"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	call := func(text string) string {
		t.Helper()
		callCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
		if err != nil {
			t.Fatalf("call %q: %v", text, err)
		}
		if res.IsError {
			t.Fatalf("call %q failed: %+v", text, res.Content)
		}
		out, _ := json.Marshal(res.StructuredContent)
		return string(out)
	}
	if got := call("before"); !strings.Contains(got, "before") {
		t.Fatalf("echo = %s", got)
	}

	// The daemon restarts: the client carries on without noticing.
	stop()
	stop = serve()
	if got := call("after"); !strings.Contains(got, "after") {
		t.Fatalf("echo after the restart = %s", got)
	}
}

func TestIDKeyNormalizes(t *testing.T) {
	if idKey(json.RawMessage(`1`)) != idKey(json.RawMessage(` 1 `)) {
		t.Fatal("whitespace must not matter")
	}
	if idKey(json.RawMessage(`"1"`)) == idKey(json.RawMessage(`1`)) {
		t.Fatal("a string id is not a number id")
	}
}
