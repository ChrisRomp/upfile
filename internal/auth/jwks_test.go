package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestValidRS512SignatureIsRejectedBeforeKeyFetch(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": "rsa-key", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(server.Close)
	payload, err := json.Marshal(map[string]any{
		"iss": server.URL, "aud": testAudience, "sub": "account-id",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS512","kid":"rsa-key"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha512.Sum512([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA512, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA512, digest[:], signature); err != nil {
		t.Fatalf("test signature is not valid: %v", err)
	}
	v, err := New(context.Background(), server.URL, testAudience, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	mustReject(t, v, input+"."+base64.RawURLEncoding.EncodeToString(signature))
	if requests.Load() != 0 {
		t.Fatal("unsupported algorithm reached the key fetch")
	}
}

func TestJWKSRequestsNeverFollowRedirects(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.Server.URL+"/cdn-cgi/access/certs", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	clock := newTestClock()
	claims := validClaims(f, clock)
	claims["iss"] = redirect.URL
	v, err := newCloudflare(context.Background(), redirect.URL, testAudience, redirect.Client(), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	mustReject(t, v, sign(t, f, claims))
	if f.Requests() != 0 {
		t.Fatal("JWKS fetch followed an untrusted redirect")
	}
}

func TestAssertionKeyURLsAreIgnored(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	var untrustedRequests atomic.Int64
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		untrustedRequests.Add(1)
		http.Error(w, "not an approved key source", http.StatusInternalServerError)
	}))
	t.Cleanup(untrusted.Close)

	claims := validClaims(f, clock)
	claims["iss"] = untrusted.URL
	claims["jwks_uri"] = untrusted.URL + "/keys"
	mustReject(t, v, sign(t, f, claims))
	legitimate := sign(t, f, validClaims(f, clock))
	for _, field := range []string{"jku", "x5u", "jwks_uri"} {
		header := map[string]string{"alg": "RS256", "kid": "unknown", field: untrusted.URL + "/keys"}
		mustReject(t, v, replaceJWTPart(t, legitimate, 0, header))
	}
	mustVerify(t, v, legitimate)
	if untrustedRequests.Load() != 0 {
		t.Fatal("assertion selected an untrusted key URL")
	}
}

func TestJWKSSizeAndMalformedResponseLimits(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"oversized response", strings.Repeat("x", maxJWKSBytes+1)},
		{"malformed JSON", `{"keys":`},
		{"empty key set", `{"keys":[]}`},
		{"malformed key", `{"keys":[{"kty":"RSA","n":"invalid!","e":"AQAB"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			v, err := newCloudflare(context.Background(), server.URL, testAudience, server.Client(), clock.now)
			if err != nil {
				t.Fatal(err)
			}
			claims := validClaims(f, clock)
			claims["iss"] = server.URL
			mustReject(t, v, sign(t, f, claims))
		})
	}
}

func TestJWKSNetworkTimeoutIsBounded(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 30 * time.Millisecond
	v, err := newCloudflare(context.Background(), server.URL, testAudience, client, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims(f, clock)
	claims["iss"] = server.URL
	start := time.Now()
	mustReject(t, v, sign(t, f, claims))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("JWKS timeout took %s", elapsed)
	}
}

func TestJWKSClientBoundsAndEndpointRestriction(t *testing.T) {
	t.Parallel()
	for _, timeout := range []time.Duration{0, time.Hour, time.Second} {
		supplied := &http.Client{Timeout: timeout}
		client := newJWKSClient(supplied, "https://team.example/cdn-cgi/access/certs", time.Now)
		want := timeout
		if want <= 0 || want > jwksRequestTimeout {
			want = jwksRequestTimeout
		}
		if client.Timeout != want || supplied.Timeout != timeout {
			t.Fatalf("client timeout = %s; supplied = %s", client.Timeout, supplied.Timeout)
		}
	}
	var calls atomic.Int64
	supplied := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected network access")
		}),
	}
	client := newJWKSClient(supplied, "https://team.example/cdn-cgi/access/certs", time.Now)
	for _, address := range []string{
		"http://team.example/cdn-cgi/access/certs",
		"https://attacker.example/cdn-cgi/access/certs",
		"https://team.example/different-path",
		"https://team.example/cdn-cgi/access/certs?jwks=other",
	} {
		if _, err := client.Get(address); err == nil {
			t.Fatalf("accepted unconfigured JWKS URL %q", address)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("transport requested an unconfigured endpoint")
	}
}

func TestExpiredCacheDoesNotReuseKeysDuringRefreshCooldown(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	token := sign(t, f, validClaims(f, clock))
	mustVerify(t, v, token)
	clock.advance(jwksCacheTTL - time.Second)
	unknown := replaceJWTPart(t, token, 0, map[string]string{"alg": "RS256", "kid": "unknown"})
	mustReject(t, v, unknown)
	clock.advance(time.Second)
	mustReject(t, v, token)
	if got := f.Requests(); got != 2 {
		t.Fatalf("cache expiry bypassed refresh limit: %d requests", got)
	}
	clock.advance(jwksRefreshInterval)
	mustVerify(t, v, token)
}
