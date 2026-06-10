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

// testSigner returns a tokenSigner on key (a random per-boot key when empty),
// failing the test instead of returning an error.
func testSigner(t *testing.T, key string) *tokenSigner {
	t.Helper()
	s, err := newTokenSigner(key)
	if err != nil {
		t.Fatalf("new token signer: %v", err)
	}
	return s
}

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
	r := newRegistry("https://gw.example.com", 16, testSigner(t, ""))

	s1, reply1, _, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	s2, reply2, _, err := r.claim(tunnel.RegisterRequest{})
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
	r := newRegistry("https://gw", 16, testSigner(t, ""))

	s, reply, isNew, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !isNew {
		t.Fatal("a first-time claim must be reported as new")
	}
	r.bind(s, fakeTunnel(t))
	if r.lookup(s.publicID) == nil {
		t.Fatal("session should be routable after bind")
	}

	// Reconnect with the secret -> same session + same public URL, NOT new.
	s2, reply2, isNew2, err := r.claim(tunnel.RegisterRequest{SessionID: reply.SessionID})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if isNew2 {
		t.Fatal("a reclaim must not be reported as new")
	}
	if s2 != s || reply2.PublicURL != reply.PublicURL {
		t.Fatalf("reclaim should return the same session and URL: %q vs %q", reply2.PublicURL, reply.PublicURL)
	}

	// An unknown secret mints a fresh session (does not error).
	s3, reply3, isNew3, err := r.claim(tunnel.RegisterRequest{SessionID: "not-a-real-secret"})
	if err != nil {
		t.Fatalf("claim unknown: %v", err)
	}
	if !isNew3 {
		t.Fatal("an unknown secret must mint a NEW session")
	}
	if s3 == s || reply3.PublicURL == reply.PublicURL {
		t.Fatal("unknown secret should mint a fresh session")
	}
}

// TestRegistryRepliesCarrySessionToken verifies every successful claim — fresh
// and reclaim alike — returns a session token the registry's own signer minted
// for that session's ids.
func TestRegistryRepliesCarrySessionToken(t *testing.T) {
	r := newRegistry("https://gw", 16, testSigner(t, "k"))

	s, reply, _, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claims, ok := r.signer.verify(reply.SessionToken)
	if !ok {
		t.Fatal("fresh claim must return a verifiable session token")
	}
	if claims.Secret != s.secret || claims.PublicID != s.publicID {
		t.Fatal("token claims must carry the session's ids")
	}

	// An in-memory reclaim returns a (fresh) token too.
	_, reply2, _, err := r.claim(tunnel.RegisterRequest{SessionID: reply.SessionID})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if c2, ok := r.signer.verify(reply2.SessionToken); !ok || c2.Secret != s.secret {
		t.Fatal("reclaim must return a verifiable token for the same session")
	}
}

// TestRegistryTokenResurrectsSessionAcrossRestart is the stable-URL property of
// issue #120: a fresh registry (a restarted gateway) holding the SAME signing
// key resurrects a session — same secret, same public URL — from the session
// token alone.
func TestRegistryTokenResurrectsSessionAcrossRestart(t *testing.T) {
	const key = "stable-signing-key"
	r1 := newRegistry("https://gw", 16, testSigner(t, key))
	_, reply1, _, err := r1.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// "Restart": a brand-new registry that knows nothing, with the same key.
	r2 := newRegistry("https://gw", 16, testSigner(t, key))
	s2, reply2, isNew, err := r2.claim(tunnel.RegisterRequest{
		SessionID:    reply1.SessionID,
		SessionToken: reply1.SessionToken,
	})
	if err != nil {
		t.Fatalf("resurrect claim: %v", err)
	}
	if reply2.PublicURL != reply1.PublicURL || reply2.SessionID != reply1.SessionID {
		t.Fatalf("resurrected session changed: url %q -> %q, id changed=%v",
			reply1.PublicURL, reply2.PublicURL, reply2.SessionID != reply1.SessionID)
	}
	if !isNew {
		t.Fatal("a resurrected session is a new reservation (must be releasable on a failed handshake)")
	}

	// A second registration with the same token (e.g. a duplicate dial) reclaims
	// the now-live entry rather than minting another.
	s3, reply3, isNew3, err := r2.claim(tunnel.RegisterRequest{SessionToken: reply1.SessionToken})
	if err != nil {
		t.Fatalf("duplicate resurrect: %v", err)
	}
	if isNew3 || s3 != s2 || reply3.PublicURL != reply1.PublicURL {
		t.Fatal("a second token claim must reclaim the resurrected session in memory")
	}
}

// TestRegistryTokenFromDifferentKeyMintsFresh: a token signed under another key
// (rotated key, or the random per-boot default) is ignored — the daemon just
// gets a fresh session, never an error.
func TestRegistryTokenFromDifferentKeyMintsFresh(t *testing.T) {
	r1 := newRegistry("https://gw", 16, testSigner(t, "key-a"))
	_, reply1, _, err := r1.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	r2 := newRegistry("https://gw", 16, testSigner(t, "key-b"))
	_, reply2, isNew, err := r2.claim(tunnel.RegisterRequest{
		SessionID:    reply1.SessionID,
		SessionToken: reply1.SessionToken,
	})
	if err != nil {
		t.Fatalf("claim with foreign token: %v", err)
	}
	if !isNew || reply2.PublicURL == reply1.PublicURL {
		t.Fatal("a token under a different key must mint a FRESH session")
	}
	// And the fresh reply's token is signed with the new key (self-healing).
	if _, ok := r2.signer.verify(reply2.SessionToken); !ok {
		t.Fatal("fresh reply must carry a token under the current key")
	}
}

// TestRegistryTokenResurrectionRespectsCapacity: a resurrected session occupies
// a slot like any other, so the MaxSessions cap still holds.
func TestRegistryTokenResurrectionRespectsCapacity(t *testing.T) {
	const key = "cap-key"
	r1 := newRegistry("https://gw", 1, testSigner(t, key))
	_, reply1, _, err := r1.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The "restarted" gateway is already full.
	r2 := newRegistry("https://gw", 1, testSigner(t, key))
	if _, _, _, err := r2.claim(tunnel.RegisterRequest{}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, _, _, err := r2.claim(tunnel.RegisterRequest{SessionToken: reply1.SessionToken}); err != errAtCapacity {
		t.Fatalf("resurrection past the cap: want errAtCapacity, got %v", err)
	}
}

func TestRegistryReclaimReplacesAndClosesOldTunnel(t *testing.T) {
	r := newRegistry("https://gw", 16, testSigner(t, ""))
	s, reply, _, _ := r.claim(tunnel.RegisterRequest{})
	old := fakeTunnel(t)
	r.bind(s, old)

	s2, _, _, _ := r.claim(tunnel.RegisterRequest{SessionID: reply.SessionID})
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
	r := newRegistry("https://gw", 1, testSigner(t, ""))
	s, _, _, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	r.bind(s, fakeTunnel(t))

	if _, _, _, err := r.claim(tunnel.RegisterRequest{}); err != errAtCapacity {
		t.Fatalf("want errAtCapacity, got %v", err)
	}
}

// TestRegistryCapacityIsHardAtClaim guards the DoS fix from PR review: the cap
// must hold even for concurrent FIRST-TIME claims that haven't bound yet, because
// the slot is reserved (in bySecret) at claim, not at bind.
func TestRegistryCapacityIsHardAtClaim(t *testing.T) {
	r := newRegistry("https://gw", 1, testSigner(t, ""))
	if _, _, _, err := r.claim(tunnel.RegisterRequest{}); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	// No bind() yet — the second claim must STILL be rejected.
	if _, _, _, err := r.claim(tunnel.RegisterRequest{}); err != errAtCapacity {
		t.Fatalf("cap must be enforced at claim (pre-bind), got %v", err)
	}
}

// TestRegistryReleaseFreesReservation verifies a failed-handshake reservation is
// freed (and frees the cap slot), while release is a no-op for a bound session.
func TestRegistryReleaseFreesReservation(t *testing.T) {
	r := newRegistry("https://gw", 1, testSigner(t, ""))
	s, _, isNew, _ := r.claim(tunnel.RegisterRequest{})
	if !isNew {
		t.Fatal("want new")
	}
	r.release(s) // handshake failed before bind
	// The slot is free again.
	if _, _, _, err := r.claim(tunnel.RegisterRequest{}); err != nil {
		t.Fatalf("slot should be free after release, got %v", err)
	}

	// release is a no-op once bound.
	r2 := newRegistry("https://gw", 16, testSigner(t, ""))
	bs, _, _, _ := r2.claim(tunnel.RegisterRequest{})
	r2.bind(bs, fakeTunnel(t))
	r2.release(bs)
	if r2.lookup(bs.publicID) == nil {
		t.Fatal("release must not remove a bound session")
	}
}

// TestRegistryReapsAbandonedReservation verifies the reaper backstop frees a
// reservation whose handshake never completed.
func TestRegistryReapsAbandonedReservation(t *testing.T) {
	r := newRegistry("https://gw", 16, testSigner(t, ""))
	s, _, _, _ := r.claim(tunnel.RegisterRequest{}) // never bound, never released
	// Within the reservation grace: kept.
	r.reap(s.createdAt.Add(reservationGrace/2), sessionTTL)
	if _, ok := r.bySecret[s.secret]; !ok {
		t.Fatal("reservation must be kept within the grace window")
	}
	// Past it: reaped.
	r.reap(s.createdAt.Add(2*reservationGrace), sessionTTL)
	if _, ok := r.bySecret[s.secret]; ok {
		t.Fatal("abandoned reservation must be reaped after the grace window")
	}
}

func TestRegistryReapHonorsGraceTTL(t *testing.T) {
	r := newRegistry("https://gw", 16, testSigner(t, ""))
	const ttl = 5 * time.Minute
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A LIVE session is never reaped.
	live, _, _, _ := r.claim(tunnel.RegisterRequest{})
	r.bind(live, fakeTunnel(t))
	r.reap(now, ttl)
	if r.lookup(live.publicID) == nil {
		t.Fatal("a live session must not be reaped")
	}

	// A closed session is kept within the grace window, then evicted after it.
	dead, _, _, _ := r.claim(tunnel.RegisterRequest{})
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
