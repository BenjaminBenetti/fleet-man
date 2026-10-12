package netprobe

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAddress(t *testing.T) {
	for in, want := range map[string]string{
		"http://fs-alpha-1a2b3c4d-aptcache:3142": "fs-alpha-1a2b3c4d-aptcache:3142",
		"http://cache/":                          "cache:80",
		"HTTPS://cache.example":                  "cache.example:443",
		"http://[fd00::1]:3142/x":                "[fd00::1]:3142",
	} {
		if got, err := Address(in); err != nil || got != want {
			t.Errorf("Address(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "cache:3142", "ftp://cache", "http://", "http://cache:port", "::"} {
		if got, err := Address(bad); err == nil {
			t.Errorf("Address(%q) = %q, want an error", bad, got)
		}
	}
}

// listener is a TCP port that accepts, and the address of one that refuses.
func listener(t *testing.T) (open, closed string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed = gone.Addr().String()
	_ = gone.Close()
	return l.Addr().String(), closed
}

func TestFirst(t *testing.T) {
	open, closed := listener(t)
	up, down := "http://"+open, "http://"+closed
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		urls []string
		want string
	}{
		{"answers", []string{up}, up},
		{"refused", []string{down}, Direct},
		{"the first that answers", []string{down, up}, up},
		{"in order", []string{up + "/a", up + "/b"}, up + "/a"},
	} {
		got, err := First(ctx, tt.urls, time.Second, 0)
		if err != nil || got != tt.want {
			t.Errorf("%s: First(%v) = %q, %v; want %q", tt.name, tt.urls, got, err, tt.want)
		}
	}
	if _, err := First(ctx, []string{up, "nope"}, time.Second, 0); err == nil {
		t.Error("a malformed URL was probed past")
	}
	if _, err := First(ctx, nil, time.Second, 0); err == nil {
		t.Error("no URL answered")
	}
}

// stubDial makes every dial answer as answer does for that address's nth
// dial (from 1); it returns how many dials each address got.
func stubDial(t *testing.T, answer func(ctx context.Context, address string, n int) error) map[string]int {
	t.Helper()
	orig, origPause := dial, retryPause
	t.Cleanup(func() { dial, retryPause = orig, origPause })
	retryPause = 10 * time.Millisecond
	var mu sync.Mutex
	dials := map[string]int{}
	dial = func(ctx context.Context, address string) (net.Conn, error) {
		mu.Lock()
		dials[address]++
		n := dials[address]
		mu.Unlock()
		if err := answer(ctx, address, n); err != nil {
			return nil, err
		}
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	return dials
}

// dropped is a try whose packets are lost: it waits out its timeout.
func dropped(ctx context.Context) error {
	<-ctx.Done()
	return errors.New("dropped")
}

// A host that never answers costs the wait (each try its timeout), then
// Direct.
func TestFirstTimesOut(t *testing.T) {
	dials := stubDial(t, func(ctx context.Context, _ string, _ int) error { return dropped(ctx) })
	start := time.Now()
	got, err := First(context.Background(), []string{"http://cache:3142"}, 50*time.Millisecond, 300*time.Millisecond)
	if err != nil || got != Direct {
		t.Fatalf("First = %q, %v; want %q", got, err, Direct)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 2*time.Second {
		t.Fatalf("took %v; want the 300ms wait", d)
	}
	if n := dials["cache:3142"]; n < 3 {
		t.Fatalf("%d tries in a 300ms wait of 50ms tries; want it tried again", n)
	}
}

// One try lost under load (a dropped DNS packet or SYN) is not the cache
// gone: the next round reaches it, and apt keeps the fleet's cache. Before
// the retry, one lost try sent apt direct (the itest 716 flake).
func TestFirstTriesAgainAfterALostTry(t *testing.T) {
	for _, tt := range []struct {
		name string
		lost func(ctx context.Context) error
	}{
		{"dropped", dropped},
		{"refused", func(context.Context) error { return syscall.ECONNREFUSED }},
		{"no such host", func(context.Context) error {
			return &net.DNSError{Err: "no such host", Name: "cache", IsNotFound: true}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dials := stubDial(t, func(ctx context.Context, _ string, n int) error {
				if n <= 2 {
					return tt.lost(ctx)
				}
				return nil
			})
			got, err := First(context.Background(), []string{"http://cache:3142"}, 50*time.Millisecond, 5*time.Second)
			if err != nil || got != "http://cache:3142" {
				t.Fatalf("First = %q, %v; want the cache", got, err)
			}
			if n := dials["cache:3142"]; n != 3 {
				t.Fatalf("%d tries; want 3", n)
			}
		})
	}
}

// Every round tries the URLs in order, and the first round's answer wins
// over a later round's; wait 0 is a single round.
func TestFirstRounds(t *testing.T) {
	dials := stubDial(t, func(ctx context.Context, address string, n int) error {
		if address == "b:1" || n > 1 {
			return nil
		}
		return dropped(ctx)
	})
	if got, _ := First(context.Background(), []string{"http://a:1", "http://b:1"}, 20*time.Millisecond, time.Second); got != "http://b:1" {
		t.Fatalf("First = %q; want b, which answered in the first round", got)
	}
	if dials["a:1"] != 1 || dials["b:1"] != 1 {
		t.Fatalf("dials = %v; want one round", dials)
	}

	dials = stubDial(t, func(ctx context.Context, _ string, _ int) error { return syscall.ECONNREFUSED })
	if got, _ := First(context.Background(), []string{"http://a:1"}, 20*time.Millisecond, 0); got != Direct {
		t.Fatalf("First = %q; want Direct", got)
	}
	if dials["a:1"] != 1 {
		t.Fatalf("wait 0 tried %d times; want once", dials["a:1"])
	}
}

// A cancelled probe stops trying.
func TestFirstCancelled(t *testing.T) {
	stubDial(t, func(ctx context.Context, _ string, _ int) error { return syscall.ECONNREFUSED })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got, _ := First(ctx, []string{"http://a:1"}, 20*time.Millisecond, time.Minute); got != Direct {
		t.Fatalf("First = %q; want Direct", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v after the cancel", d)
	}
}
