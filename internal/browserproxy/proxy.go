// Package browserproxy is `fleet browser-proxy`, the HTTP proxy that the
// host's browser (TUI 'b') reaches an instance's own localhost through. It is
// built into the fleet binary staged in every instance, so it needs no root,
// no package manager and no egress: it replaced the privoxy fleet used to
// install.
//
// It is a plain forward proxy for development servers, not a filter: a
// request in absolute form is sent on with its headers as they came (several
// Set-Cookie headers in an answer stay several, which is why fleet left
// tinyproxy for privoxy, and no X-Forwarded-For is added), bodies stream both
// ways unbuffered (chunked uploads, server-sent events), an Upgrade (a
// WebSocket) is relayed, and CONNECT tunnels to any port (HTTPS and
// WebSockets from a browser), passing each side's half-close on. Only the
// hop-by-hop headers a proxy must not forward are dropped.
package browserproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"sync"
	"time"
)

// DefaultPort is where the proxy listens in an instance; the TUI forwards a
// local port to it.
const DefaultPort = 58888

// dialTimeout bounds a connect to a target.
const dialTimeout = 30 * time.Second

// Config is how the proxy serves.
type Config struct {
	// Addr is the address to listen on ("" listens on every interface);
	// Port the port (DefaultPort when 0).
	Addr string
	Port int
	// Log receives one line per failed request and the listening line
	// (stderr when nil).
	Log io.Writer
}

// Run serves the proxy until ctx ends. Once it listens it logs
// "fleet browser-proxy: listening on <addr>", which the daemon waits for.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	logger := log.New(cfg.Log, "fleet browser-proxy: ", log.LstdFlags)
	srv := &http.Server{
		Handler:           New(logger),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          logger,
	}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	fmt.Fprintf(cfg.Log, "fleet browser-proxy: listening on %s\n", ln.Addr())
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// New is the proxy's handler; logger hears about failed requests (nil
// discards).
func New(logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// A proxy of the instance's environment is never chained to: the
		// targets are the instance's own services.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Encodings pass as they are: the browser asked for them.
		DisableCompression: true,
	}
	p := &proxy{dial: dialer.DialContext, logger: logger}
	p.forward = &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			// The request names its target (absolute form): it goes there
			// as it came. A missing X-Forwarded-For stays missing.
			if _, ok := out.Header["X-Forwarded-For"]; !ok {
				out.Header["X-Forwarded-For"] = nil
			}
		},
		Transport: transport,
		// Stream every byte as it comes (hot reload, server-sent events).
		FlushInterval: -1,
		ErrorLog:      logger,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Printf("%s %s: %v", r.Method, r.URL, err)
			http.Error(w, fmt.Sprintf("fleet browser-proxy: %s: %v", r.URL.Host, err), http.StatusBadGateway)
		},
	}
	return p
}

type proxy struct {
	forward *httputil.ReverseProxy
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	logger  *log.Logger
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "fleet browser-proxy: not a proxy request (the request line names no absolute URL)", http.StatusBadRequest)
		return
	}
	if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
		http.Error(w, fmt.Sprintf("fleet browser-proxy: unsupported scheme %q", r.URL.Scheme), http.StatusBadRequest)
		return
	}
	p.forward.ServeHTTP(w, r)
}

// connect tunnels a CONNECT to its target: the reply, then bytes both ways.
func (p *proxy) connect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if _, port, err := net.SplitHostPort(target); err != nil || port == "" {
		http.Error(w, fmt.Sprintf("fleet browser-proxy: CONNECT %q names no host:port", target), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		p.logger.Printf("CONNECT %s: %v", target, err)
		http.Error(w, fmt.Sprintf("fleet browser-proxy: %s: %v", target, err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "fleet browser-proxy: this connection cannot be tunnelled", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		p.logger.Printf("CONNECT %s: %v", target, err)
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	splice(client, buf.Reader, upstream)
}

// splice copies client (whose first bytes may sit in its buffered reader)
// and upstream both ways until both directions end, passing each EOF on as
// a half-close, then closes both.
func splice(client net.Conn, clientR *bufio.Reader, upstream net.Conn) {
	defer client.Close()
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(upstream, clientR)
		if !closeWrite(upstream) || err != nil {
			_ = upstream.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(client, upstream)
		if !closeWrite(client) || err != nil {
			_ = client.Close()
		}
	}()
	wg.Wait()
}

// closeWrite half-closes c when it can, and reports whether it did.
func closeWrite(c net.Conn) bool {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() == nil
	}
	return false
}
