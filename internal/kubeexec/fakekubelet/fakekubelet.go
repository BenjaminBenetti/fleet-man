//go:build !nok8s

// Package fakekubelet is a stand-in for an API server's pods/portforward
// endpoint and the kubelet behind it, for tests of fleet's port-forward
// clients: it upgrades to SPDY as the API server does (a WebSocket-tunnelled
// upgrade is refused, so a client falls back to plain SPDY as it would
// through a proxy that cannot upgrade), pairs each error and data stream by
// request id, and connects each pair to the local address the test routes the
// pod port to. A port with no route is refused on the error stream with the
// runtime's words, as the kubelet does; a forbidden endpoint answers 403. Only
// test code imports it.
package fakekubelet

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
)

// Kubelet is one fake API server and kubelet.
type Kubelet struct {
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[int]string
	forbid   bool
	early    time.Duration
	requests int
	upgrades int
	streams  int
	conns    map[httpstream.Connection]bool
	pods     map[string]int
}

// New starts a fake kubelet; Close stops it.
func New() *Kubelet {
	k := &Kubelet{routes: map[int]string{}, conns: map[httpstream.Connection]bool{}, pods: map[string]int{}}
	k.srv = httptest.NewServer(http.HandlerFunc(k.serve))
	return k
}

// Config is a client configuration that reaches the fake.
func (k *Kubelet) Config() *rest.Config { return &rest.Config{Host: k.srv.URL} }

// Route connects the pod's port to addr (a local listener).
func (k *Kubelet) Route(port int, addr string) {
	k.mu.Lock()
	k.routes[port] = addr
	k.mu.Unlock()
}

// Forbid makes every later upgrade answer 403 Forbidden (no
// pods/portforward permission).
func (k *Kubelet) Forbid(on bool) {
	k.mu.Lock()
	k.forbid = on
	k.mu.Unlock()
}

// EarlyClose ends a forwarded connection d after the client half-closes it,
// as containerd does (about a second): a reply later than that is lost. Zero
// keeps a true half-close.
func (k *Kubelet) EarlyClose(d time.Duration) {
	k.mu.Lock()
	k.early = d
	k.mu.Unlock()
}

// Requests is how many port-forward upgrades were asked for (a WebSocket
// attempt and its SPDY fallback count as one each; refused ones included).
func (k *Kubelet) Requests() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.requests
}

// Upgrades is how many port-forward connections were upgraded.
func (k *Kubelet) Upgrades() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.upgrades
}

// Streams is how many forwarded connections were made.
func (k *Kubelet) Streams() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.streams
}

// UpgradesFor is how many connections were upgraded for namespace/pod.
func (k *Kubelet) UpgradesFor(namespace, pod string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.pods[namespace+"/"+pod]
}

// Open is how many upgraded connections are still open.
func (k *Kubelet) Open() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.conns)
}

// Drop closes every upgraded connection (an API server rollout).
func (k *Kubelet) Drop() {
	k.mu.Lock()
	conns := make([]httpstream.Connection, 0, len(k.conns))
	for c := range k.conns {
		conns = append(conns, c)
	}
	k.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Close drops every connection and stops the server.
func (k *Kubelet) Close() {
	k.Drop()
	k.srv.CloseClientConnections()
	k.srv.Close()
}

func (k *Kubelet) serve(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	// api/v1/namespaces/<ns>/pods/<pod>/portforward
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "namespaces" || parts[4] != "pods" || parts[6] != "portforward" {
		http.NotFound(w, req)
		return
	}
	ns, pod := parts[3], parts[5]
	k.mu.Lock()
	forbid := k.forbid
	k.requests++
	k.mu.Unlock()
	if forbid {
		status := metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
			Message: fmt.Sprintf(`pods "%s" is forbidden: User "test" cannot create resource "pods/portforward" in API group "" in the namespace "%s"`, pod, ns),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(status)
		return
	}
	if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "this server does not tunnel SPDY over WebSockets", http.StatusBadRequest)
		return
	}
	if _, err := httpstream.Handshake(req, w, []string{"portforward.k8s.io"}); err != nil {
		return
	}
	streams := make(chan httpstream.Stream, 16)
	conn := spdy.NewResponseUpgrader().UpgradeResponse(w, req, func(s httpstream.Stream, _ <-chan struct{}) error {
		streams <- s
		return nil
	})
	if conn == nil {
		return
	}
	k.mu.Lock()
	k.upgrades++
	k.pods[ns+"/"+pod]++
	k.conns[conn] = true
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.conns, conn)
		k.mu.Unlock()
	}()

	type pair struct{ errStream, data httpstream.Stream }
	pairs := map[string]*pair{}
	for {
		select {
		case <-conn.CloseChan():
			return
		case s := <-streams:
			id := s.Headers().Get(corev1.PortForwardRequestIDHeader)
			p := pairs[id]
			if p == nil {
				p = &pair{}
				pairs[id] = p
			}
			switch s.Headers().Get(corev1.StreamType) {
			case corev1.StreamTypeError:
				p.errStream = s
			case corev1.StreamTypeData:
				p.data = s
			default:
				_ = s.Reset()
				continue
			}
			if p.errStream != nil && p.data != nil {
				delete(pairs, id)
				go k.forward(conn, p.errStream, p.data)
			}
		}
	}
}

// forward connects one stream pair to its port's route.
func (k *Kubelet) forward(conn httpstream.Connection, errStream, data httpstream.Stream) {
	defer conn.RemoveStreams(errStream, data)
	defer errStream.Close()
	defer data.Close()
	port, _ := strconv.Atoi(data.Headers().Get(corev1.PortHeader))
	k.mu.Lock()
	addr, ok := k.routes[port]
	early := k.early
	k.streams++
	k.mu.Unlock()
	refuse := func(err error) {
		fmt.Fprintf(errStream, "error forwarding port %d to pod: failed to connect to localhost:%d inside namespace: %v", port, port, err)
	}
	if !ok {
		refuse(fmt.Errorf("dial tcp4 127.0.0.1:%d: connect: connection refused", port))
		return
	}
	tcp, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		refuse(err)
		return
	}
	defer tcp.Close()
	up := make(chan struct{})
	go func() {
		defer close(up)
		_, _ = io.Copy(tcp, data)
		if cw, ok := tcp.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	down := make(chan struct{})
	go func() {
		defer close(down)
		_, _ = io.Copy(data, tcp)
		// The peer is done: the client reads EOF.
		_ = data.Close()
	}()
	select {
	case <-down:
		<-up
	case <-up:
		if early > 0 {
			select {
			case <-down:
			case <-time.After(early):
				// The runtime gives up on the connection: what the peer
				// answers later never reaches the client.
				_ = data.Reset()
				_ = tcp.Close()
			}
			return
		}
		<-down
	}
}

// Listen starts a local TCP server handling each connection with serve, and
// returns its address; it stops when the test's cleanup closes it.
func Listen(serve func(net.Conn)) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				serve(c)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, nil
}
