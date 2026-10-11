//go:build !nok8s

package kubeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// portforward.go is the pods/portforward client: one upgraded connection to a
// pod (WebSocket-tunnelled SPDY, falling back to plain SPDY as kubectl does),
// on which every TCP connection to one of the pod's ports is a stream pair, an
// error stream and a data stream. Closing the data stream's write side sends
// its FIN to the kubelet (whether the pod's peer reads EOF is the runtime's
// business: containerd does not pass it on, and ends the connection a second
// later); the kubelet answers a refused port on the error stream. The connection lives as long as its owner keeps it: one connection
// carries any number of TCP connections, so a pool of them costs one API
// server upgrade per pod.

// portForwardDialTimeout bounds the upgrade when the caller's ctx has no
// deadline of its own.
const portForwardDialTimeout = 30 * time.Second

// portErrorWait bounds how long a data stream's end waits for its error
// stream to say whether the pod refused the port.
const portErrorWait = 5 * time.Second

// ErrPortForwardForbidden is a port-forward the API server refused for
// authorization (no pods/portforward permission).
var ErrPortForwardForbidden = errors.New("the cluster forbids port-forwarding to the pod (pods/portforward)")

// IsForbidden reports whether err is the API server refusing an upgrade (an
// exec or a port-forward) with 403 Forbidden.
func IsForbidden(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrPortForwardForbidden) || apierrors.IsForbidden(err) {
		return true
	}
	// An upgrade failure keeps the API server's status as its cause, which
	// it does not unwrap.
	var upgrade *httpstream.UpgradeFailureError
	return errors.As(err, &upgrade) && IsForbidden(upgrade.Cause)
}

// PortForward is one port-forward connection to a pod.
type PortForward struct {
	conn      httpstream.Connection
	namespace string
	pod       string
	nextID    atomic.Int64

	mu        sync.Mutex
	active    int
	idleSince time.Time
	done      chan struct{}
	closeOnce sync.Once
}

// PortForward opens a port-forward connection to pod with the connection's
// pinned configuration.
func (c *Connection) PortForward(ctx context.Context, namespace, pod string) (*PortForward, error) {
	return DialPortForward(ctx, c.RESTConfig(), namespace, pod)
}

// PortForward opens a port-forward connection to pod over this transport.
func (t *Transport) PortForward(ctx context.Context, namespace, pod string) (*PortForward, error) {
	return DialPortForward(ctx, t.cfg, namespace, pod)
}

// DialPortForward opens a port-forward connection to pod with cfg. ctx bounds
// the upgrade (30s when it has no deadline); the connection outlives it.
func DialPortForward(ctx context.Context, cfg *rest.Config, namespace, pod string) (*PortForward, error) {
	u, err := portForwardURL(cfg, namespace, pod)
	if err != nil {
		return nil, err
	}
	dialer, err := portForwardDialer(cfg, u)
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, portForwardDialTimeout)
		defer cancel()
	}
	type dialed struct {
		conn httpstream.Connection
		err  error
	}
	// The dialers take no context: an upgrade the caller gave up on is
	// closed when it lands.
	result := make(chan dialed, 1)
	go func() {
		conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		result <- dialed{conn, err}
	}()
	select {
	case r := <-result:
		if r.err != nil {
			if IsForbidden(r.err) {
				return nil, fmt.Errorf("%w: %v", ErrPortForwardForbidden, r.err)
			}
			return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, r.err)
		}
		return newPortForward(r.conn, namespace, pod), nil
	case <-ctx.Done():
		go func() {
			if r := <-result; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, ctx.Err())
	}
}

func newPortForward(conn httpstream.Connection, namespace, pod string) *PortForward {
	f := &PortForward{conn: conn, namespace: namespace, pod: pod, idleSince: time.Now(), done: make(chan struct{})}
	go func() {
		<-conn.CloseChan()
		f.closeOnce.Do(func() { close(f.done) })
	}()
	return f
}

// portForwardURL is the pods/portforward URL of pod.
func portForwardURL(cfg *rest.Config, namespace, pod string) (*url.URL, error) {
	if namespace == "" || pod == "" {
		return nil, errors.New("port-forward: no namespace or pod")
	}
	core := *cfg
	core.APIPath = "/api"
	core.GroupVersion = &schema.GroupVersion{Version: "v1"}
	base, versioned, err := rest.DefaultServerUrlFor(&core)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	u := *base
	u.Path = path.Join("/", base.Path, versioned, "namespaces", namespace, "pods", pod, "portforward")
	return &u, nil
}

// portForwardDialer is kubectl's: SPDY tunnelled over a WebSocket, else plain
// SPDY when the API server or a proxy cannot upgrade to a WebSocket. A
// refusal for authorization is final (the other protocol is refused alike).
func portForwardDialer(cfg *rest.Config, u *url.URL) (httpstream.Dialer, error) {
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("port-forward over spdy: %w", err)
	}
	direct := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	tunnel, err := portforward.NewSPDYOverWebsocketDialer(u, cfg)
	if err != nil {
		return nil, fmt.Errorf("port-forward over websocket: %w", err)
	}
	return portforward.NewFallbackDialer(tunnel, direct, func(err error) bool {
		return (httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)) && !IsForbidden(err)
	}), nil
}

// Done is closed when the connection ends (the API server or the kubelet
// closed it, a ping failed, or Close).
func (f *PortForward) Done() <-chan struct{} { return f.done }

// Alive reports whether the connection is still up.
func (f *PortForward) Alive() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

// Close ends the connection and every stream on it.
func (f *PortForward) Close() error {
	err := f.conn.Close()
	f.closeOnce.Do(func() { close(f.done) })
	return err
}

// Active is how many TCP connections are open on the connection.
func (f *PortForward) Active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// IdleSince is when the connection's last TCP connection ended; zero while
// one is open.
func (f *PortForward) IdleSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active > 0 {
		return time.Time{}
	}
	return f.idleSince
}

// Dial opens a TCP connection to port on the pod's loopback: one stream pair.
// A port nothing listens on is answered on the connection's error stream, so
// it shows as the first Read failing with a *PortError (the connection itself
// stays up for the others).
func (f *PortForward) Dial(port int) (*PortConn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port-forward: invalid port %d", port)
	}
	if !f.Alive() {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", f.namespace, f.pod, ErrPortForwardClosed)
	}
	id := strconv.FormatInt(f.nextID.Add(1), 10)
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, id)
	errStream, err := f.conn.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s:%d: %w (%v)", f.namespace, f.pod, port, ErrPortForwardClosed, err)
	}
	// Nothing is ever written to the error stream.
	_ = errStream.Close()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := f.conn.CreateStream(headers)
	if err != nil {
		f.conn.RemoveStreams(errStream)
		return nil, fmt.Errorf("port-forward to %s/%s:%d: %w (%v)", f.namespace, f.pod, port, ErrPortForwardClosed, err)
	}
	c := &PortConn{f: f, port: port, data: data, errStream: errStream, errDone: make(chan struct{})}
	go c.readError()
	f.mu.Lock()
	f.active++
	f.mu.Unlock()
	return c, nil
}

// ErrPortForwardClosed is a dial on a port-forward connection that has
// ended: the caller opens a new one.
var ErrPortForwardClosed = errors.New("the port-forward connection is closed")

// PortError is the kubelet refusing a forwarded connection (nothing listens
// on the port in the pod, say).
type PortError struct {
	Port    int
	Message string
}

func (e *PortError) Error() string {
	return fmt.Sprintf("port %d in the pod: %s", e.Port, e.Message)
}

// PortConn is one forwarded TCP connection. Reads and writes are the data
// stream; CloseWrite half-closes the stream (a runtime that passes it on
// gives the pod's peer an EOF it can still answer); Close ends it both ways.
type PortConn struct {
	f         *PortForward
	port      int
	data      httpstream.Stream
	errStream httpstream.Stream

	errDone chan struct{}
	errMsg  string

	closeOnce sync.Once
}

func (c *PortConn) readError() {
	defer close(c.errDone)
	msg, _ := io.ReadAll(io.LimitReader(c.errStream, 64<<10))
	c.errMsg = string(msg)
}

// Read reads what the pod's peer sent. The end of it is io.EOF, or the
// kubelet's *PortError when it refused the connection.
func (c *PortConn) Read(p []byte) (int, error) {
	n, err := c.data.Read(p)
	if errors.Is(err, io.EOF) {
		if perr := c.refusal(); perr != nil {
			return n, perr
		}
	}
	return n, err
}

// refusal is the error stream's message once it ends (bounded wait).
func (c *PortConn) refusal() error {
	select {
	case <-c.errDone:
	case <-time.After(portErrorWait):
		return nil
	}
	if c.errMsg == "" {
		return nil
	}
	return &PortError{Port: c.port, Message: c.errMsg}
}

func (c *PortConn) Write(p []byte) (int, error) { return c.data.Write(p) }

// CloseWrite sends the stream's FIN.
func (c *PortConn) CloseWrite() error { return c.data.Close() }

// Close ends the connection both ways and frees its streams.
func (c *PortConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.data.Reset()
		_ = c.errStream.Reset()
		c.f.conn.RemoveStreams(c.data, c.errStream)
		c.f.mu.Lock()
		c.f.active--
		if c.f.active == 0 {
			c.f.idleSince = time.Now()
		}
		c.f.mu.Unlock()
	})
	return nil
}
