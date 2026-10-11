package netprobe

import (
	"context"
	"errors"
	"net"
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
		got, err := First(ctx, tt.urls, time.Second)
		if err != nil || got != tt.want {
			t.Errorf("%s: First(%v) = %q, %v; want %q", tt.name, tt.urls, got, err, tt.want)
		}
	}
	if _, err := First(ctx, []string{up, "nope"}, time.Second); err == nil {
		t.Error("a malformed URL was probed past")
	}
	if _, err := First(ctx, nil, time.Second); err == nil {
		t.Error("no URL answered")
	}
}

// A host that never answers costs the timeout, then Direct.
func TestFirstTimesOut(t *testing.T) {
	orig := dial
	t.Cleanup(func() { dial = orig })
	dial = func(ctx context.Context, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, errors.New("dropped")
	}
	start := time.Now()
	got, err := First(context.Background(), []string{"http://cache:3142"}, 50*time.Millisecond)
	if err != nil || got != Direct {
		t.Fatalf("First = %q, %v; want %q", got, err, Direct)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v past a 50ms timeout", d)
	}
}
