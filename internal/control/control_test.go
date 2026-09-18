package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector is a concurrency-safe sink for envelopes received by a Server's
// handler. The handler may run on multiple goroutines, so every access is
// guarded; wait blocks until at least n envelopes have arrived (or the test
// times out), avoiding flaky sleeps.
type collector struct {
	mu   sync.Mutex
	got  []Envelope
	cond *sync.Cond
}

func newCollector() *collector {
	c := &collector{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *collector) handle(env Envelope) {
	c.mu.Lock()
	c.got = append(c.got, env)
	c.cond.Broadcast()
	c.mu.Unlock()
}

// wait blocks until at least n envelopes have been received or the deadline
// passes, returning a copy of what arrived.
func (c *collector) wait(t *testing.T, n int, timeout time.Duration) []Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.got) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// sync.Cond has no timed wait; arm a timer that wakes the wait so we
		// re-check the deadline rather than blocking forever on a missed
		// broadcast.
		timer := time.AfterFunc(remaining, func() {
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		})
		c.cond.Wait()
		timer.Stop()
	}
	out := make([]Envelope, len(c.got))
	copy(out, c.got)
	return out
}

// tempSocket returns a socket path inside a fresh temp dir. Not t.TempDir():
// on macOS the /var/folders/... test temp dirs (plus the test name) can push
// the path past the platform's 104-byte sun_path limit, failing the bind
// with EINVAL.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fmctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, SocketName)
}

// TestRoundTripOpenBrowser dials a listening Server, sends an OpenBrowser
// message, and asserts the handler receives the correct Type and a payload
// that decodes back to the original URL.
func TestRoundTripOpenBrowser(t *testing.T) {
	c := newCollector()
	srv, err := Listen(tempSocket(t), c.handle)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(srv.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	const want = "http://localhost:3000"
	if err := cli.OpenBrowser(want); err != nil {
		t.Fatalf("OpenBrowser: %v", err)
	}

	got := c.wait(t, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("received %d envelopes, want 1", len(got))
	}
	if got[0].Type != TypeOpenBrowser {
		t.Errorf("Type = %q, want %q", got[0].Type, TypeOpenBrowser)
	}
	var p OpenBrowserPayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.URL != want {
		t.Errorf("URL = %q, want %q", p.URL, want)
	}
}

// TestRoundTripCopyFile round-trips a CopyFile message and asserts the payload
// carries both endpoints and the `fleet open` flag verbatim.
func TestRoundTripCopyFile(t *testing.T) {
	c := newCollector()
	srv, err := Listen(tempSocket(t), c.handle)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(srv.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	if err := cli.CopyFile(":build/chart.png", "host:~/Pictures/", true); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	got := c.wait(t, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("received %d envelopes, want 1", len(got))
	}
	if got[0].Type != TypeCopyFile {
		t.Errorf("Type = %q, want %q", got[0].Type, TypeCopyFile)
	}
	var p CopyFilePayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.Src != ":build/chart.png" || p.Dst != "host:~/Pictures/" || !p.Open {
		t.Errorf("payload = %+v, want src/dst verbatim and open=true", p)
	}
}

// TestMultipleMessagesOneConnection verifies several Envelopes sent over a
// single connection are each decoded and dispatched in order.
func TestMultipleMessagesOneConnection(t *testing.T) {
	c := newCollector()
	srv, err := Listen(tempSocket(t), c.handle)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(srv.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	urls := []string{"http://a", "http://b", "http://c"}
	for _, u := range urls {
		if err := cli.OpenBrowser(u); err != nil {
			t.Fatalf("OpenBrowser(%q): %v", u, err)
		}
	}

	got := c.wait(t, len(urls), 2*time.Second)
	if len(got) != len(urls) {
		t.Fatalf("received %d envelopes, want %d", len(got), len(urls))
	}
	for i, env := range got {
		var p OpenBrowserPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("decode payload %d: %v", i, err)
		}
		if p.URL != urls[i] {
			t.Errorf("envelope %d URL = %q, want %q", i, p.URL, urls[i])
		}
	}
}

// TestConcurrentClientsNoCrossTalk proves that two independently-dialled
// clients sending at the same time do NOT corrupt or interleave each other's
// messages. Each Dial is a distinct stream connection that the Server reads
// with its own json.Decoder, so bytes from one client can never bleed into
// another's frame. If they could, the JSON would desync and either fail to
// decode or yield a URL that matches neither client's prefix — both of which
// this test would catch. (Multiple clients are the realistic case: two
// `fleet launch` processes running in the same instance at once.)
func TestConcurrentClientsNoCrossTalk(t *testing.T) {
	c := newCollector()
	srv, err := Listen(tempSocket(t), c.handle)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	const perClient = 50
	// Two clients send concurrently, each tagging every URL with its own
	// prefix so a crossed byte stream would surface as a decode error or a
	// mismatched prefix.
	prefixes := []string{"http://client-A/", "http://client-B/"}
	var wg sync.WaitGroup
	for _, prefix := range prefixes {
		wg.Add(1)
		go func(prefix string) {
			defer wg.Done()
			cli, err := Dial(srv.socketPath)
			if err != nil {
				t.Errorf("Dial(%s): %v", prefix, err)
				return
			}
			defer cli.Close()
			for i := 0; i < perClient; i++ {
				if err := cli.OpenBrowser(prefix + strconv.Itoa(i)); err != nil {
					t.Errorf("OpenBrowser(%s%d): %v", prefix, i, err)
					return
				}
			}
		}(prefix)
	}
	wg.Wait()

	got := c.wait(t, len(prefixes)*perClient, 5*time.Second)
	if len(got) != len(prefixes)*perClient {
		t.Fatalf("received %d envelopes, want %d", len(got), len(prefixes)*perClient)
	}

	// Every envelope must decode cleanly and carry exactly one client's prefix
	// with a parseable index — no garbled frames, no lost or duplicated sends.
	counts := map[string]int{}
	for i, env := range got {
		if env.Type != TypeOpenBrowser {
			t.Fatalf("envelope %d Type = %q, want %q", i, env.Type, TypeOpenBrowser)
		}
		var p OpenBrowserPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("envelope %d failed to decode (stream cross-talk?): %v", i, err)
		}
		matched := false
		for _, prefix := range prefixes {
			if suffix, ok := strings.CutPrefix(p.URL, prefix); ok {
				if _, err := strconv.Atoi(suffix); err != nil {
					t.Fatalf("envelope %d URL %q has non-numeric suffix (corruption?)", i, p.URL)
				}
				counts[prefix]++
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("envelope %d URL %q matches no client prefix (cross-talk?)", i, p.URL)
		}
	}
	for _, prefix := range prefixes {
		if counts[prefix] != perClient {
			t.Errorf("client %q delivered %d messages, want %d", prefix, counts[prefix], perClient)
		}
	}
}

// TestListenReplacesStaleSocket verifies Listen succeeds even when a leftover
// file already exists at the socket path (e.g. from a crashed prior run): the
// stale file is removed before listening.
func TestListenReplacesStaleSocket(t *testing.T) {
	path := tempSocket(t)
	if err := os.WriteFile(path, []byte("stale"), 0o666); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	srv, err := Listen(path, func(Envelope) {})
	if err != nil {
		t.Fatalf("Listen over stale file: %v", err)
	}
	defer srv.Close()

	// And the listener is genuinely usable.
	cli, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial after stale replacement: %v", err)
	}
	cli.Close()
}

// TestCloseRemovesSocketFile verifies Close deletes the socket file it
// created, leaving the path clean for the next Listen.
func TestCloseRemovesSocketFile(t *testing.T) {
	path := tempSocket(t)
	srv, err := Listen(path, func(Envelope) {})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file missing while listening: %v", err)
	}

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still present after Close (stat err = %v), want not-exist", err)
	}

	// A second Close is a no-op.
	if err := srv.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestDialNoServerErrors verifies Dial against a path with no listener returns
// an error the caller can treat as "host not available".
func TestDialNoServerErrors(t *testing.T) {
	if _, err := Dial(tempSocket(t)); err == nil {
		t.Fatal("Dial with no server = nil error, want error")
	}
}
