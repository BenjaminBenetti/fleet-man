package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// token.go mints and verifies the session-resume token: a JWT, returned in
// every RegisterReply, that encodes the session's two ids and is signed
// (HMAC-SHA256) with the gateway's session key. fleetd stores it alongside the
// session secret and presents it on reconnect; because the SIGNATURE proves the
// ids were minted by a gateway holding the key, a restarted gateway — whose
// in-memory registry is empty — can resurrect the session under its original
// public URL instead of minting a fresh one (issue #120).
//
// The JWT is implemented directly on the stdlib (the gateway deliberately
// depends only on grpc + internal/tunnel): base64url(header) "."
// base64url(claims) "." base64url(HMAC(key, header "." claims)). Only the
// gateway ever signs or verifies these tokens — fleetd treats them as opaque —
// which keeps the implementation surface to one fixed algorithm.

// sessionTokenHeader is the encoded JOSE header of every session token. The
// algorithm is pinned: verify requires the header segment to match this string
// byte-for-byte, so algorithm-confusion forgeries (alg=none and friends) are
// rejected before any crypto runs.
var sessionTokenHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// sessionClaims is the JWT payload: the ids that define a session. A token is
// a bearer reclaim capability — whoever holds it can claim the session's
// public URL — so it is returned only on the register stream to the fleetd
// that owns the session (which persists it 0600, like the secret).
type sessionClaims struct {
	// Secret is the session's reclaim credential (the registry.bySecret key).
	Secret string `json:"sec"`
	// PublicID is the id in the session's public URL.
	PublicID string `json:"pub"`
	// IssuedAt is the standard iat claim (seconds since epoch). Informational
	// only: tokens do not expire — they are exactly as long-lived as the
	// session secret they carry.
	IssuedAt int64 `json:"iat"`
}

// tokenSigner signs and verifies session-resume tokens with an HMAC-SHA256 key.
type tokenSigner struct {
	key []byte
}

// newTokenSigner returns a signer for key. An empty key gets a random per-boot
// key: tokens still verify for the life of the process, but a restarted
// gateway cannot verify them, so session URLs do not survive the restart
// (the documented behavior when --session-key is not set).
func newTokenSigner(key string) (*tokenSigner, error) {
	if key != "" {
		return &tokenSigner{key: []byte(key)}, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &tokenSigner{key: b}, nil
}

// mint signs a session-resume token for a session's ids.
func (t *tokenSigner) mint(secret, publicID string) string {
	// Marshaling a struct of strings + int64 cannot fail.
	claims, _ := json.Marshal(sessionClaims{Secret: secret, PublicID: publicID, IssuedAt: time.Now().Unix()})
	signingInput := sessionTokenHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	return signingInput + "." + t.sign(signingInput)
}

// sign returns the encoded HMAC-SHA256 signature of signingInput.
func (t *tokenSigner) sign(signingInput string) string {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks token's shape and signature and returns its claims. ok is
// false for anything this signer's key did not mint: a missing/malformed
// token, a header other than the pinned HS256 one, a bad signature, or claim
// ids that are not 256-bit hex (belt-and-braces — the claims feed registry map
// keys and the public URL path, so they must be exactly id-shaped).
func (t *tokenSigner) verify(token string) (sessionClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != sessionTokenHeader {
		return sessionClaims{}, false
	}
	if !hmac.Equal([]byte(t.sign(parts[0]+"."+parts[1])), []byte(parts[2])) {
		return sessionClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sessionClaims{}, false
	}
	var c sessionClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return sessionClaims{}, false
	}
	if !isHexID(c.Secret) || !isHexID(c.PublicID) {
		return sessionClaims{}, false
	}
	return c, true
}

// isHexID reports whether s is shaped like an id newID mints: 64 hex chars
// (256 bits).
func isHexID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
