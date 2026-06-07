package gateway

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
)

// fakeTunnel returns a live yamux session (the gateway/server end) backed by an
// in-memory pipe, so registry tests can bind real sessions without a network.
func fakeTunnel(t *testing.T) *yamux.Session {
	t.Helper()
	c1, c2 := net.Pipe()
	srv, err := tunnel.ServerSession(c1, io.Discard)
	if err != nil {
		t.Fatalf("server session: %v", err)
	}
	cli, err := tunnel.ClientSession(c2, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = cli.Close()
		_ = c1.Close()
		_ = c2.Close()
	})
	return srv
}

func TestRegistryClaimMintsDistinctUnguessableIDs(t *testing.T) {
	r := newRegistry("https://gw.example.com", 16)

	s1, reply1, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	s2, reply2, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}

	if s1.secret == s2.secret || s1.publicID == s2.publicID {
		t.Fatal("distinct claims must get distinct secret and public ids")
	}
	if len(s1.secret) != 64 || len(s1.publicID) != 64 {
		t.Fatalf("ids should be 256-bit hex (64 chars): secret=%d public=%d", len(s1.secret), len(s1.publicID))
	}
	// The headline security property: the reclaim secret must NEVER appear in the
	// public URL (which agents can see).
	if strings.Contains(reply1.PublicURL, s1.secret) || strings.Contains(reply2.PublicURL, s2.secret) {
		t.Fatal("public URL must not contain the reclaim secret")
	}
	if reply1.PublicURL != "https://gw.example.com/mcp/"+s1.publicID {
		t.Fatalf("unexpected public url: %q", reply1.PublicURL)
	}
	// A fresh (unbound) claim is not yet routable.
	if r.lookup(s1.publicID) != nil {
		t.Fatal("session should not be routable before bind")
	}
}

func TestRegistryReclaimReturnsSameURL(t *testing.T) {
	r := newRegistry("https://gw", 16)

	s, reply, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	r.bind(s, fakeTunnel(t))
	if r.lookup(s.publicID) == nil {
		t.Fatal("session should be routable after bind")
	}

	// Reconnect with the secret -> same session + same public URL.
	s2, reply2, err := r.claim(tunnel.RegisterRequest{SessionID: reply.SessionID})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if s2 != s || reply2.PublicURL != reply.PublicURL {
		t.Fatalf("reclaim should return the same session and URL: %q vs %q", reply2.PublicURL, reply.PublicURL)
	}

	// An unknown secret mints a fresh session (does not error).
	s3, reply3, err := r.claim(tunnel.RegisterRequest{SessionID: "not-a-real-secret"})
	if err != nil {
		t.Fatalf("claim unknown: %v", err)
	}
	if s3 == s || reply3.PublicURL == reply.PublicURL {
		t.Fatal("unknown secret should mint a fresh session")
	}
}

func TestRegistryReclaimReplacesAndClosesOldTunnel(t *testing.T) {
	r := newRegistry("https://gw", 16)
	s, reply, _ := r.claim(tunnel.RegisterRequest{})
	old := fakeTunnel(t)
	r.bind(s, old)

	s2, _, _ := r.claim(tunnel.RegisterRequest{SessionID: reply.SessionID})
	newYm := fakeTunnel(t)
	r.bind(s2, newYm)

	if !old.IsClosed() {
		t.Fatal("reclaim should close the superseded tunnel")
	}
	if s.currentYamux() != newYm {
		t.Fatal("session should now point at the new tunnel")
	}
}

func TestRegistryCapacity(t *testing.T) {
	r := newRegistry("https://gw", 1)
	s, _, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	r.bind(s, fakeTunnel(t))

	if _, _, err := r.claim(tunnel.RegisterRequest{}); err != errAtCapacity {
		t.Fatalf("want errAtCapacity, got %v", err)
	}
}

func TestRegistryReapHonorsGraceTTL(t *testing.T) {
	r := newRegistry("https://gw", 16)
	const ttl = 5 * time.Minute
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A LIVE session is never reaped.
	live, _, _ := r.claim(tunnel.RegisterRequest{})
	r.bind(live, fakeTunnel(t))
	r.reap(now, ttl)
	if r.lookup(live.publicID) == nil {
		t.Fatal("a live session must not be reaped")
	}

	// A closed session is kept within the grace window, then evicted after it.
	dead, _, _ := r.claim(tunnel.RegisterRequest{})
	deadYm := fakeTunnel(t)
	r.bind(dead, deadYm)
	_ = deadYm.Close()

	r.reap(now, ttl) // first observation: stamps closedAt, keeps it
	if r.lookup(dead.publicID) == nil {
		t.Fatal("a just-closed session must be kept within the grace window (for sticky reconnect)")
	}
	r.reap(now.Add(ttl/2), ttl) // still within grace
	if r.lookup(dead.publicID) == nil {
		t.Fatal("session must remain reserved within the grace window")
	}
	r.reap(now.Add(2*ttl), ttl) // past grace -> evicted
	if r.lookup(dead.publicID) != nil {
		t.Fatal("session must be reaped after the grace TTL")
	}
}
