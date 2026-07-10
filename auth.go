package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authenticator is the process-wide auth handle. nil means auth is disabled,
// in which case /callback is turned off entirely. Mirrors the defaultPinger
// global in pinger.go.
var authenticator *Authenticator

// Authenticator issues and verifies the service's own JWTs using an ephemeral
// Ed25519 keypair generated at startup. It signs and verifies with the same
// key, so no key material is ever configured or exposed; a restart invalidates
// all outstanding tokens.
type Authenticator struct {
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	lifetime time.Duration
}

func newAuthenticator(lifetime time.Duration) (*Authenticator, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate auth key: %w", err)
	}
	return &Authenticator{priv: priv, pub: pub, lifetime: lifetime}, nil
}

// issue mints a token valid for the configured lifetime from now.
func (a *Authenticator) issue(now time.Time) (token string, exp time.Time) {
	exp = now.Add(a.lifetime)
	return signToken(a.priv, claims{Sub: "1", Iat: now.Unix(), Exp: exp.Unix()}), exp
}

// verifyRequest extracts a Bearer token from the Authorization header and
// verifies it. Returns an error describing why auth failed.
func (a *Authenticator) verifyRequest(r *http.Request) error {
	h := r.Header.Get("Authorization")
	if h == "" {
		return fmt.Errorf("missing Authorization header")
	}
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return fmt.Errorf("expected 'Bearer <token>'")
	}
	return verifyToken(a.pub, strings.TrimSpace(tok), time.Now())
}

// rotate logs a token immediately, then a fresh one every lifetime/2 until ctx
// is cancelled, so a still-valid token is always visible in recent logs.
func (a *Authenticator) rotate(ctx context.Context, lg *logger) {
	a.logToken(lg)
	interval := a.lifetime / 2
	if interval <= 0 {
		interval = a.lifetime
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.logToken(lg)
		}
	}
}

func (a *Authenticator) logToken(lg *logger) {
	token, exp := a.issue(time.Now())
	lg.infof("auth token (expires %s): %s", exp.Format(time.RFC3339), token)
}
