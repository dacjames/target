package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func validClaims(now time.Time) claims {
	return claims{Sub: "1", Iat: now.Unix(), Exp: now.Add(time.Hour).Unix()}
}

func TestJWTRoundTrip(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	tok := signToken(priv, validClaims(now))
	if err := verifyToken(pub, tok, now); err != nil {
		t.Fatalf("verify valid token: %v", err)
	}
}

func TestJWTExpired(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	tok := signToken(priv, claims{Sub: "1", Iat: now.Add(-2 * time.Hour).Unix(), Exp: now.Add(-time.Hour).Unix()})
	if err := verifyToken(pub, tok, now); err == nil {
		t.Fatal("expected expired token to fail")
	}
}

func TestJWTTamperedPayload(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	tok := signToken(priv, validClaims(now))
	parts := strings.Split(tok, ".")
	// Re-encode altered claims, keep original signature.
	altered := b64.EncodeToString([]byte(`{"sub":"1","iat":0,"exp":9999999999}`))
	tampered := parts[0] + "." + altered + "." + parts[2]
	if err := verifyToken(pub, tampered, now); err == nil {
		t.Fatal("expected tampered payload to fail signature check")
	}
}

func TestJWTWrongKey(t *testing.T) {
	_, priv := testKeys(t)
	otherPub, _ := testKeys(t)
	now := time.Now()
	tok := signToken(priv, validClaims(now))
	if err := verifyToken(otherPub, tok, now); err == nil {
		t.Fatal("expected verification with wrong key to fail")
	}
}

func TestJWTAlgNoneRejected(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	tok := signToken(priv, validClaims(now))
	parts := strings.Split(tok, ".")
	// Swap header to alg:none; signature segment left intact.
	noneHeader := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	forged := noneHeader + "." + parts[1] + "." + parts[2]
	if err := verifyToken(pub, forged, now); err == nil {
		t.Fatal("expected alg:none header to be rejected")
	}
	// Also an empty signature with alg:none must fail.
	if err := verifyToken(pub, noneHeader+"."+parts[1]+".", now); err == nil {
		t.Fatal("expected alg:none with empty signature to be rejected")
	}
}

func TestJWTWrongSubject(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	tok := signToken(priv, claims{Sub: "2", Iat: now.Unix(), Exp: now.Add(time.Hour).Unix()})
	if err := verifyToken(pub, tok, now); err == nil {
		t.Fatal("expected non-'1' subject to fail")
	}
}

func TestJWTMalformed(t *testing.T) {
	pub, _ := testKeys(t)
	for _, tok := range []string{"", "a.b", "a.b.c.d", "not-a-token"} {
		if err := verifyToken(pub, tok, time.Now()); err == nil {
			t.Fatalf("expected malformed token %q to fail", tok)
		}
	}
}

func TestAuthenticatorIssueVerify(t *testing.T) {
	a, err := newAuthenticator(time.Hour)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	now := time.Now()
	tok, exp := a.issue(now)
	if !exp.After(now) {
		t.Fatalf("exp %v not after now %v", exp, now)
	}
	if err := verifyToken(a.pub, tok, now); err != nil {
		t.Fatalf("verify issued token: %v", err)
	}
	// Header must be exactly our fixed header.
	hdr, _ := b64.DecodeString(strings.Split(tok, ".")[0])
	var h map[string]string
	if err := json.Unmarshal(hdr, &h); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if h["alg"] != "EdDSA" || h["typ"] != "JWT" {
		t.Fatalf("header = %v, want EdDSA/JWT", h)
	}
}
