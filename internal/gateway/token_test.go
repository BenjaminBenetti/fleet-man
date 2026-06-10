package gateway

import (
	"encoding/base64"
	"strings"
	"testing"
)

// hexID returns a valid 64-char hex id built from a short label, for tests.
func hexID(label byte) string {
	return strings.Repeat(string("0123456789abcdef"[label%16]), 64)
}

func TestTokenMintVerifyRoundTrip(t *testing.T) {
	s := testSigner(t, "round-trip-key")
	sec, pub := hexID(1), hexID(2)

	tok := s.mint(sec, pub)
	if tok == "" {
		t.Fatal("mint returned an empty token")
	}
	claims, ok := s.verify(tok)
	if !ok {
		t.Fatal("freshly-minted token must verify")
	}
	if claims.Secret != sec || claims.PublicID != pub {
		t.Fatalf("claims round trip: got (%q,%q), want (%q,%q)", claims.Secret, claims.PublicID, sec, pub)
	}
	if claims.IssuedAt == 0 {
		t.Fatal("iat claim should be stamped")
	}
}

// TestTokenSameKeyAcrossSigners is the restart property: a signer built later
// with the SAME key (a new gateway process) verifies tokens an earlier one
// minted.
func TestTokenSameKeyAcrossSigners(t *testing.T) {
	tok := testSigner(t, "shared-key").mint(hexID(1), hexID(2))
	if _, ok := testSigner(t, "shared-key").verify(tok); !ok {
		t.Fatal("a token must verify across signer instances sharing the key")
	}
	if _, ok := testSigner(t, "different-key").verify(tok); ok {
		t.Fatal("a token must NOT verify under a different key")
	}
}

// TestTokenRandomKeyPerBoot: two signers with no configured key (the default)
// must not trust each other's tokens — that is exactly the documented
// no---session-key restart behavior.
func TestTokenRandomKeyPerBoot(t *testing.T) {
	a, b := testSigner(t, ""), testSigner(t, "")
	tok := a.mint(hexID(1), hexID(2))
	if _, ok := a.verify(tok); !ok {
		t.Fatal("a random-key signer must verify its own tokens")
	}
	if _, ok := b.verify(tok); ok {
		t.Fatal("two random-key signers must not share trust")
	}
}

func TestTokenVerifyRejectsForgeries(t *testing.T) {
	s := testSigner(t, "forgery-key")
	valid := s.mint(hexID(1), hexID(2))
	parts := strings.Split(valid, ".")

	b64 := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	otherClaims := b64(`{"sec":"` + hexID(3) + `","pub":"` + hexID(4) + `","iat":1}`)

	cases := map[string]string{
		"empty":               "",
		"garbage":             "not-a-token",
		"two segments":        parts[0] + "." + parts[1],
		"four segments":       valid + ".extra",
		"swapped claims":      parts[0] + "." + otherClaims + "." + parts[2],
		"truncated signature": parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2],
		"alg none":            b64(`{"alg":"none","typ":"JWT"}`) + "." + parts[1] + ".",
		"alg RS256":           b64(`{"alg":"RS256","typ":"JWT"}`) + "." + parts[1] + "." + parts[2],
		"claims not base64":   parts[0] + ".!!!." + parts[2],
	}
	for name, tok := range cases {
		if _, ok := s.verify(tok); ok {
			t.Errorf("%s: forged token must not verify", name)
		}
	}
}

// TestTokenVerifyRejectsNonIDClaims: even a correctly-signed token is rejected
// when its ids are not 256-bit hex — the claims feed registry map keys and the
// public URL path, so they must be exactly id-shaped.
func TestTokenVerifyRejectsNonIDClaims(t *testing.T) {
	s := testSigner(t, "shape-key")
	for name, tok := range map[string]string{
		"path traversal public id": s.mint(hexID(1), "../../evil"),
		"short secret":             s.mint("abc123", hexID(2)),
		"empty ids":                s.mint("", ""),
		"non-hex chars":            s.mint(hexID(1), strings.Repeat("z", 64)),
	} {
		if _, ok := s.verify(tok); ok {
			t.Errorf("%s: token with non-id claims must not verify", name)
		}
	}
}
