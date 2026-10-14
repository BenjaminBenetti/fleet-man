//go:build !nok8s

package kubeexec

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// silentAPIServer takes connections and never answers them: an API server
// that is down behind an address that still accepts, or a hung one.
func silentAPIServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	return l.Addr().String()
}

// An exec to an API server that never answers fails once its connection's
// bound is out (it used to wait for the kernel's connect retries, over two
// minutes), coded unreachable, on either exec protocol.
func TestExecToASilentAPIServerEndsAtItsConnectBound(t *testing.T) {
	defer func(d time.Duration) { execConnectTimeout = d }(execConnectTimeout)
	execConnectTimeout = 300 * time.Millisecond
	addr := silentAPIServer(t)
	cfg := &rest.Config{Host: "https://" + addr, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	for _, protocol := range []string{ProtocolAuto, ProtocolWebSocket, ProtocolSPDY} {
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- ExecConfig(context.Background(), cfg, ExecRequest{Namespace: "team", Pod: "fi-x-0", Command: []string{"true"}, Protocol: protocol},
				Stdio{Stdout: io.Discard, Stderr: io.Discard})
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), kubeerr.ErrNoAnswer.Error()) || kubeerr.Transport(err) != kubeerr.Unreachable {
				t.Errorf("protocol %q: exec to a silent API server = %v, want ErrNoAnswer (unreachable)", protocol, err)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("protocol %q: took %s", protocol, took)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("protocol %q: an exec to a silent API server is still waiting", protocol)
		}
	}
}

// The bound covers the round trip only: an answer's body stays readable
// until it is closed, and a caller that gives up is not taken for a silent
// server.
func TestBoundedRoundTrip(t *testing.T) {
	var reqCtx context.Context
	rt := boundedRoundTrip{d: time.Minute, rt: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reqCtx = r.Context()
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("refused"))}, nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://api/x", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if reqCtx.Err() != nil {
		t.Fatal("the bound ended before the answer was read")
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "refused" {
		t.Fatalf("body %q", body)
	}
	_ = resp.Body.Close()
	if reqCtx.Err() == nil {
		t.Fatal("closing the answer did not end the bound")
	}

	gaveUp, cancel := context.WithCancel(context.Background())
	cancel()
	rt.rt = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	req, _ = http.NewRequestWithContext(gaveUp, http.MethodGet, "https://api/x", nil)
	if _, err := rt.RoundTrip(req); errors.Is(err, kubeerr.ErrNoAnswer) {
		t.Fatalf("a cancelled request reads as a silent server: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
