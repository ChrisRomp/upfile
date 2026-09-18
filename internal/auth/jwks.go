package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	jwksCacheTTL        = 5 * time.Minute
	jwksRefreshInterval = 30 * time.Second
	jwksRequestTimeout  = 5 * time.Second
	maxJWKSBytes        = 1024 * 1024
)

// RemoteKeySet already coalesces fetches and caches keys, but has neither an
// expiration nor an unknown-key refresh rate limit. These wrappers add both.
type expiringKeySet struct {
	mu        sync.Mutex
	remote    *oidc.RemoteKeySet
	expiresAt time.Time
	now       func() time.Time
	newRemote func() *oidc.RemoteKeySet
}

func (k *expiringKeySet) VerifySignature(ctx context.Context, token string) ([]byte, error) {
	k.mu.Lock()
	if k.remote == nil || !k.now().Before(k.expiresAt) {
		k.remote = k.newRemote()
		k.expiresAt = k.now().Add(jwksCacheTTL)
	}
	remote := k.remote
	k.mu.Unlock()
	// Expired keys are deliberately discarded even when the endpoint is down.
	return remote.VerifySignature(ctx, token)
}

type jwksTransport struct {
	base    http.RoundTripper
	url     string
	now     func() time.Time
	mu      sync.Mutex
	next    time.Time
	credits int
}

func newJWKSClient(supplied *http.Client, jwksURL string, now func() time.Time) *http.Client {
	client := &http.Client{}
	if supplied != nil {
		*client = *supplied
	}
	if client.Timeout <= 0 || client.Timeout > jwksRequestTimeout {
		client.Timeout = jwksRequestTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Jar = nil
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &jwksTransport{base: base, url: jwksURL, now: now}
	return client
}

func (t *jwksTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.String() != t.url {
		return nil, errors.New("cloudflare: JWKS request does not match the configured endpoint")
	}
	t.mu.Lock()
	now := t.now()
	if t.next.IsZero() {
		// Permit the initial load plus one immediate key rotation. Thereafter
		// all unknown-key/signature-failure fetches share a single rate limit.
		t.credits = 2
		t.next = now.Add(jwksRefreshInterval)
	} else if !now.Before(t.next) {
		t.credits = 1
		t.next = now.Add(jwksRefreshInterval)
	}
	if t.credits == 0 {
		t.mu.Unlock()
		return nil, errors.New("cloudflare: JWKS refresh temporarily rate limited")
	}
	t.credits--
	t.mu.Unlock()

	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare: JWKS endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cloudflare: reading JWKS: %w", err)
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("cloudflare: JWKS response exceeds the size limit")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return response, nil
}
