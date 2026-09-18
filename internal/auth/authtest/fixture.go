// Package authtest provides an isolated, TLS-backed Cloudflare Access fixture.
// Signing keys are generated for each instance. Import it only in tests or an
// explicitly selected local development harness, never in the release server.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Issuer serves its public keys over loopback HTTPS and signs Access-shaped JWTs.
// Use Server.Client() to trust its certificate without disabling TLS checks.
type Issuer struct {
	Server *httptest.Server

	mu          sync.RWMutex
	key         *rsa.PrivateKey
	generation  uint64
	unavailable bool
	requests    atomic.Int64
}

// New generates a fresh RSA key and starts a loopback-only HTTPS JWKS server.
func New() (*Issuer, error) {
	f := &Issuer{}
	if err := f.Rotate(); err != nil {
		return nil, err
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serveHTTP))
	return f, nil
}

// Close shuts down the fixture.
func (f *Issuer) Close() { f.Server.Close() }

// Rotate replaces the published signing key; previously signed tokens remain
// useful for testing cache expiry and key removal.
func (f *Issuer) Rotate() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("auth fixture: generating signing key: %w", err)
	}
	f.mu.Lock()
	f.key = key
	f.generation++
	f.mu.Unlock()
	return nil
}

// SetUnavailable makes the JWKS endpoint return 503 until restored.
func (f *Issuer) SetUnavailable(unavailable bool) {
	f.mu.Lock()
	f.unavailable = unavailable
	f.mu.Unlock()
}

// Requests reports how many times the JWKS endpoint has been requested.
func (f *Issuer) Requests() int64 { return f.requests.Load() }

// Token signs an assertion valid for ten minutes for the supplied identity and
// audience. Sign permits tests to supply specific claims and temporal bounds.
func (f *Issuer) Token(subject, email, audience string) (string, error) {
	now := time.Now()
	return f.Sign(map[string]any{
		"iss":   f.Server.URL,
		"aud":   []string{audience},
		"sub":   subject,
		"email": email,
		"iat":   now.Unix(),
		"exp":   now.Add(10 * time.Minute).Unix(),
	})
}

// Sign signs exactly the supplied claims using RS256.
func (f *Issuer) Sign(claims map[string]any) (string, error) {
	f.mu.RLock()
	key, kid := f.key, f.keyID()
	f.mu.RUnlock()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", fmt.Errorf("auth fixture: encoding JWT header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("auth fixture: encoding claims: %w", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("auth fixture: signing JWT: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (f *Issuer) keyID() string { return "fixture-" + strconv.FormatUint(f.generation, 10) }

func (f *Issuer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/cdn-cgi/access/certs" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	f.requests.Add(1)
	f.mu.RLock()
	unavailable := f.unavailable
	key, kid := f.key, f.keyID()
	f.mu.RUnlock()
	if unavailable {
		http.Error(w, "fixture JWKS unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": kid,
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}},
	})
}
