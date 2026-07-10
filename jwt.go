package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Minimal inlined JWT — the subset we use, nothing more: one fixed secure
// configuration (EdDSA / Ed25519), no algorithm agility. Modeled on
// github.com/golang-jwt/jwt but stripped to sign + verify with a single alg.

// jwtHeaderB64 is the base64url encoding of the one and only accepted header,
// {"alg":"EdDSA","typ":"JWT"}. Verification compares the token's header segment
// to this constant byte-for-byte, which structurally rejects "alg":"none" and
// algorithm-confusion attacks: there is no algorithm to negotiate.
var jwtHeaderB64 = b64.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))

// b64 is JWT's base64url alphabet without padding.
var b64 = base64.RawURLEncoding

// claims is the fixed claim set: subject, issued-at, expiry (Unix seconds).
type claims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// signToken produces a signed compact JWT for the given claims.
func signToken(priv ed25519.PrivateKey, c claims) string {
	payload, _ := json.Marshal(c)
	signingInput := jwtHeaderB64 + "." + b64.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + b64.EncodeToString(sig)
}

// verifyToken validates a compact JWT against pub at time now. It returns nil
// only for a well-formed, correctly-signed, unexpired token with sub=="1".
func verifyToken(pub ed25519.PublicKey, token string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("malformed token")
	}
	// Fixed-config gate: only our exact header is acceptable.
	if parts[0] != jwtHeaderB64 {
		return fmt.Errorf("unexpected token header")
	}

	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("bad signature encoding")
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(pub, []byte(signingInput), sig) {
		return fmt.Errorf("signature verification failed")
	}

	rawClaims, err := b64.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("bad claims encoding")
	}
	var c claims
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return fmt.Errorf("bad claims: %w", err)
	}
	if c.Sub != "1" {
		return fmt.Errorf("unexpected subject")
	}
	nowUnix := now.Unix()
	if nowUnix >= c.Exp {
		return fmt.Errorf("token expired")
	}
	if c.Iat > nowUnix+60 { // allow small forward clock skew only
		return fmt.Errorf("token not yet valid")
	}
	return nil
}
