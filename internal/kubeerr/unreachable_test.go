package kubeerr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// refusedGet is what client-go returns for a GET to an API server nobody
// listens for: a *url.Error around the dial's *net.OpError.
func refusedGet(t *testing.T) error {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	resp, err := http.Get("http://" + addr + "/apis/apps/v1/namespaces/team/statefulsets/fi-x")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("GET %s answered", addr)
	}
	return err
}

// endedDial is the error of a dial to an address that never answers whose
// context ended first: with an expired deadline it is the "i/o timeout"
// that Is context.DeadlineExceeded, which net also gives about half the
// times a dialer's own Timeout ends such a dial.
func endedDial(t *testing.T, ctx context.Context) error {
	t.Helper()
	_, err := (&net.Dialer{}).DialContext(ctx, "tcp", "192.0.2.1:443")
	if err == nil {
		t.Fatal("a dial with an ended context connected")
	}
	return err
}

func TestUnreached(t *testing.T) {
	refused := refusedGet(t)
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	timedOut := endedDial(t, expired)
	if !errors.Is(timedOut, context.DeadlineExceeded) {
		t.Fatalf("the timed out dial %v is not a deadline", timedOut)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		err  error
		code Code
	}{
		"refused GET": {fmt.Errorf("instance StatefulSet team/fi-x: %w", refused), Unreachable},
		"reset":       {fmt.Errorf("stream: %w", os.NewSyscallError("read", syscall.ECONNRESET)), Unreachable},
		"no route":    {&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, Unreachable},
		"dns":         {&url.Error{Op: "Get", URL: "https://api.lab.invalid", Err: &net.DNSError{Err: "no such host", Name: "api.lab.invalid", IsNotFound: true}}, Unreachable},
		// A host that never answers: no connection was made, whichever
		// deadline ended the dial.
		"dial timeout": {&url.Error{Op: "Get", URL: "https://192.0.2.1:443/api", Err: timedOut}, Unreachable},
		"dial cancel":  {&url.Error{Op: "Get", URL: "https://192.0.2.1:443/api", Err: endedDial(t, cancelled)}, ""},
		// A code it already has wins (an expired login read off a 401).
		"coded": {Wrap(Unauthorized, refused), Unauthorized},
		// The caller gave up, or its deadline passed: not the cluster's doing.
		"cancelled": {&url.Error{Op: "Get", URL: "https://api", Err: context.Canceled}, ""},
		"deadline":  {fmt.Errorf("verify: %w", &url.Error{Op: "Get", URL: "https://api", Err: context.DeadlineExceeded}), ""},
		"plain":     {errors.New("exit status 1"), ""},
	} {
		got := Unreached(tc.err)
		code, ok := Of(got)
		if !ok {
			code = ""
		}
		if code != tc.code {
			t.Errorf("%s: Unreached(%v) has code %q, want %q", name, tc.err, code, tc.code)
			continue
		}
		if tc.code == "" && got != tc.err {
			t.Errorf("%s: Unreached changed an error it has no code for: %v", name, got)
		}
		if tc.code != "" && !errors.Is(got, tc.err) {
			t.Errorf("%s: Unreached(%v) lost the error it coded", name, tc.err)
		}
	}
	if Unreached(nil) != nil {
		t.Error("Unreached(nil) is not nil")
	}
}

// The coded message starts with the code, as a user sees it, so Parse reads
// it back off the text alone (a gRPC status message, the shim's stderr).
func TestUnreachedMessageLeadsWithTheCode(t *testing.T) {
	err := Unreached(fmt.Errorf("instance StatefulSet team/fi-x: %w", refusedGet(t)))
	msg := err.Error()
	if !strings.HasPrefix(msg, string(Unreachable)+": instance StatefulSet team/fi-x: Get ") || !strings.Contains(msg, "connection refused") {
		t.Fatalf("message %q", msg)
	}
	if code, _, ok := Parse(msg); !ok || code != Unreachable {
		t.Errorf("Parse(%q) = %q, %v", msg, code, ok)
	}
}

// untrustedGet is what a GET to an API server whose certificate the client
// does not trust returns: a *url.Error around the x509 failure, which is a
// net.Error like any *url.Error.
func untrustedGet(t *testing.T) error {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	resp, err := (&http.Client{Transport: &http.Transport{}}).Get(srv.URL + "/api")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an untrusted server answered")
	}
	return err
}

// refusedClientGet is what a GET with no client certificate to an API server
// that requires one returns: the server's certificate alert.
func refusedClientGet(t *testing.T) error {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/api")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server requiring a client certificate answered without one")
	}
	return err
}

// A server that was reached but not trusted, or that refused the client's
// certificate, and a credential plugin that failed, are not an unreachable
// cluster (all three are *url.Errors, so net.Errors): they are the
// connection's settings and its login.
func TestTransportTellsReachedFromUnreached(t *testing.T) {
	untrusted, refusedClient := untrustedGet(t), refusedClientGet(t)
	plugin := &url.Error{Op: "Get", URL: "https://api.lab.invalid/api", Err: errors.New("getting credentials: exec: executable kubelogin failed with exit code 1")}
	var netErr net.Error
	for name, err := range map[string]error{"untrusted": untrusted, "refused client": refusedClient, "plugin": plugin} {
		if !errors.As(err, &netErr) {
			t.Fatalf("%s: %v is no net.Error; the case proves nothing", name, err)
		}
	}
	for name, tc := range map[string]struct {
		err  error
		code Code
	}{
		"untrusted server": {fmt.Errorf("pods/exec: %w", untrusted), Misconfigured},
		"client refused":   {refusedClient, Unauthorized},
		"plugin":           {plugin, Unauthorized},
		"refused GET":      {refusedGet(t), Unreachable},
		// Connected, no answer within the exec's bound, flattened into a
		// message by the SPDY dialer.
		"no answer": {fmt.Errorf("error sending request: %v", fmt.Errorf("%w within 30s: %w", ErrNoAnswer, context.DeadlineExceeded)), Unreachable},
		"plain":     {errors.New("exit status 1"), ""},
	} {
		if got := Transport(tc.err); got != tc.code {
			t.Errorf("%s: Transport(%v) = %q, want %q", name, tc.err, got, tc.code)
		}
		if got := IsUnreachable(tc.err); got != (tc.code == Unreachable) {
			t.Errorf("%s: IsUnreachable(%v) = %v", name, tc.err, got)
		}
		coded, _ := Of(Unreached(tc.err))
		if coded != tc.code {
			t.Errorf("%s: Unreached coded %q, want %q", name, coded, tc.code)
		}
	}
}
