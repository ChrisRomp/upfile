package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"upfile/internal/auth/authtest"
)

const testAudience = "configured-application"

type testClock struct{ seconds atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.seconds.Store(time.Now().Unix())
	return c
}

func (c *testClock) now() time.Time              { return time.Unix(c.seconds.Load(), 0) }
func (c *testClock) advance(delta time.Duration) { c.seconds.Add(int64(delta / time.Second)) }

func fixture(t *testing.T) *authtest.Issuer {
	t.Helper()
	f, err := authtest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func validClaims(f *authtest.Issuer, clock *testClock) map[string]any {
	return map[string]any{
		"iss":   f.Server.URL,
		"aud":   []string{testAudience},
		"sub":   "account-id",
		"email": "signed@example.com",
		"iat":   clock.now().Unix(),
		"exp":   clock.now().Add(time.Hour).Unix(),
	}
}

func sign(t *testing.T, f *authtest.Issuer, claims map[string]any) string {
	t.Helper()
	token, err := f.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func verifier(t *testing.T, f *authtest.Issuer, clock *testClock) *Cloudflare {
	t.Helper()
	v, err := newCloudflare(context.Background(), f.Server.URL, testAudience, f.Server.Client(), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustVerify(t *testing.T, v *Cloudflare, token string) Principal {
	t.Helper()
	principal, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func mustReject(t *testing.T, v *Cloudflare, token string) {
	t.Helper()
	principal, err := v.Verify(context.Background(), token)
	if err == nil {
		t.Fatal("accepted an invalid assertion")
	}
	if principal != (Principal{}) {
		t.Fatalf("invalid token returned a principal: %+v", principal)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, issuer := range []string{
		"", "team.cloudflareaccess.com", "http://team.cloudflareaccess.com",
		"https://", "https://user:pass@example.com", "https://example.com/team",
		"https://example.com?key=other", "https://example.com?",
		"https://example.com#fragment", "https://example.com#", "https://example.com:",
		"https://example.com:0",
		"https://example.com:65536", "https://example.com:%35",
	} {
		t.Run(issuer, func(t *testing.T) {
			if _, err := New(context.Background(), issuer, testAudience, nil); err == nil {
				t.Fatalf("accepted invalid issuer %q", issuer)
			}
		})
	}
	for _, audience := range []string{"", " ", "\t", " padded "} {
		if _, err := New(context.Background(), "https://team.cloudflareaccess.com", audience, nil); err == nil {
			t.Fatalf("accepted invalid audience %q", audience)
		}
	}
	if _, err := New(nil, "https://team.cloudflareaccess.com", testAudience, nil); err == nil {
		t.Fatal("accepted nil context")
	}
}

func TestNewIsLazyAndSupportsTrustedTLSFixture(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	client := f.Server.Client()
	v, err := New(context.Background(), f.Server.URL+"/", testAudience, client)
	if err != nil {
		t.Fatal(err)
	}
	if f.Requests() != 0 {
		t.Fatal("constructor fetched JWKS")
	}
	if client.Timeout != 0 || client.CheckRedirect != nil {
		t.Fatal("constructor modified the caller's client")
	}
	token, err := f.Token("account-id", "signed@example.com", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	want := Principal{Subject: "account-id", Email: "signed@example.com"}
	for range 5 {
		if got := mustVerify(t, v, token); got != want {
			t.Fatalf("principal = %+v, want %+v", got, want)
		}
	}
	if got := f.Requests(); got != 1 {
		t.Fatalf("cached verification made %d JWKS requests, want 1", got)
	}
}

func TestClaimsValidation(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://attacker.invalid" }},
		{"missing issuer", func(c map[string]any) { delete(c, "iss") }},
		{"wrong audience", func(c map[string]any) { c["aud"] = []string{"another-application"} }},
		{"missing audience", func(c map[string]any) { delete(c, "aud") }},
		{"empty audience", func(c map[string]any) { c["aud"] = []string{} }},
		{"expired", func(c map[string]any) { c["exp"] = clock.now().Add(-time.Second).Unix() }},
		{"expiration boundary", func(c map[string]any) { c["exp"] = clock.now().Unix() }},
		{"missing expiration", func(c map[string]any) { delete(c, "exp") }},
		{"null expiration", func(c map[string]any) { c["exp"] = nil }},
		{"string expiration", func(c map[string]any) { c["exp"] = fmt.Sprint(clock.now().Add(time.Hour).Unix()) }},
		{"not before one second", func(c map[string]any) { c["nbf"] = clock.now().Add(time.Second).Unix() }},
		{"not before distant", func(c map[string]any) { c["nbf"] = clock.now().Add(time.Hour).Unix() }},
		{"string not before", func(c map[string]any) { c["nbf"] = fmt.Sprint(clock.now().Unix()) }},
		{"future issued at", func(c map[string]any) { c["iat"] = clock.now().Add(issuedAtSkew + time.Second).Unix() }},
		{"issued after expiry", func(c map[string]any) {
			c["exp"] = clock.now().Add(10 * time.Second).Unix()
			c["iat"] = clock.now().Add(11 * time.Second).Unix()
		}},
		{"string issued at", func(c map[string]any) { c["iat"] = fmt.Sprint(clock.now().Unix()) }},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }},
		{"blank subject", func(c map[string]any) { c["sub"] = " \t" }},
		{"invalid email type", func(c map[string]any) { c["email"] = []string{"someone@example.com"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(f, clock)
			tc.change(claims)
			mustReject(t, v, sign(t, f, claims))
		})
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"single audience", func(c map[string]any) { c["aud"] = testAudience }},
		{"multiple audiences", func(c map[string]any) { c["aud"] = []string{"other", testAudience} }},
		{"not before boundary", func(c map[string]any) { c["nbf"] = clock.now().Unix() }},
		{"small issued at skew", func(c map[string]any) { c["iat"] = clock.now().Add(issuedAtSkew).Unix() }},
		{"optional issued at", func(c map[string]any) { delete(c, "iat") }},
		{"optional email", func(c map[string]any) { delete(c, "email") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(f, clock)
			tc.change(claims)
			mustVerify(t, v, sign(t, f, claims))
		})
	}
	if got := f.Requests(); got != 1 {
		t.Fatalf("claim checks triggered %d JWKS requests, want 1", got)
	}
}

func replaceJWTPart(t *testing.T, token string, part int, value any) string {
	t.Helper()
	parts := strings.Split(token, ".")
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	parts[part] = base64.RawURLEncoding.EncodeToString(data)
	return strings.Join(parts, ".")
}

func TestTamperedSignaturesAndAlgorithms(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	claims := validClaims(f, clock)
	token := sign(t, f, claims)
	mustVerify(t, v, token)

	claims["email"] = "forged@example.com"
	mustReject(t, v, replaceJWTPart(t, token, 1, claims))
	parts := strings.Split(token, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	mustReject(t, v, strings.Join(parts, "."))
	for _, algorithm := range []string{"none", "HS256", "RS384", "RS512", "ES256", ""} {
		t.Run(algorithm, func(t *testing.T) {
			mustReject(t, v, replaceJWTPart(t, token, 0, map[string]string{"alg": algorithm, "kid": "fixture-1"}))
		})
	}
	for _, invalid := range []string{"", "garbage", token + ".extra", "Bearer " + token, strings.Repeat("x", maxTokenBytes+1)} {
		mustReject(t, v, invalid)
	}
	mustVerify(t, v, token)
	if got := f.Requests(); got > 2 {
		t.Fatalf("invalid signatures bypassed refresh limit: %d requests", got)
	}
}

func TestForgedIdentityHeadersCannotEstablishIdentity(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	request := httptest.NewRequest(http.MethodGet, "https://admin.example.com/", nil)
	request.Header.Set("Cf-Access-Authenticated-User-Email", "forged@example.com")
	request.Header.Set("X-Forwarded-User", "forged-user")
	mustReject(t, v, request.Header.Get("Cf-Access-Jwt-Assertion"))
	request.Header.Set("Cf-Access-Jwt-Assertion", sign(t, f, validClaims(f, clock)))
	got := mustVerify(t, v, request.Header.Get("Cf-Access-Jwt-Assertion"))
	if got.Email != "signed@example.com" || got.Subject != "account-id" {
		t.Fatalf("unverified headers influenced identity: %+v", got)
	}
}

func TestRotationAndBoundedUnknownKeyRefresh(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	mustVerify(t, v, sign(t, f, validClaims(f, clock)))
	if err := f.Rotate(); err != nil {
		t.Fatal(err)
	}
	rotated := sign(t, f, validClaims(f, clock))
	mustVerify(t, v, rotated)
	if got := f.Requests(); got != 2 {
		t.Fatalf("key rotation made %d requests, want 2", got)
	}
	for i := range 30 {
		unknown := replaceJWTPart(t, rotated, 0, map[string]string{"alg": "RS256", "kid": fmt.Sprintf("unknown-%d", i)})
		mustReject(t, v, unknown)
	}
	if got := f.Requests(); got != 2 {
		t.Fatalf("unknown-key flood made %d requests, want 2", got)
	}
	mustVerify(t, v, rotated)
	if err := f.Rotate(); err != nil {
		t.Fatal(err)
	}
	next := sign(t, f, validClaims(f, clock))
	mustReject(t, v, next)
	clock.advance(jwksRefreshInterval)
	mustVerify(t, v, next)
	if got := f.Requests(); got != 3 {
		t.Fatalf("refresh after cooldown made %d requests, want 3", got)
	}
}

func TestCacheExpiresAndRemovedKeysAreRejected(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	old := sign(t, f, validClaims(f, clock))
	mustVerify(t, v, old)
	if err := f.Rotate(); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, v, old)
	clock.advance(jwksCacheTTL)
	mustReject(t, v, old)
	mustVerify(t, v, sign(t, f, validClaims(f, clock)))
	if got := f.Requests(); got != 2 {
		t.Fatalf("expiry made %d requests, want 2", got)
	}
}

func TestUnavailableJWKSFailsClosed(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	cached := sign(t, f, validClaims(f, clock))
	mustVerify(t, v, cached)
	f.SetUnavailable(true)
	mustVerify(t, v, cached)
	clock.advance(jwksCacheTTL)
	mustReject(t, v, cached)
	for range 10 {
		mustReject(t, v, cached)
	}
	if got := f.Requests(); got != 2 {
		t.Fatalf("outage triggered unbounded refreshes: %d", got)
	}
	f.SetUnavailable(false)
	clock.advance(jwksRefreshInterval)
	mustVerify(t, v, cached)
}

func TestUnavailableInitialKeysAndUntrustedTLS(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	token := sign(t, f, validClaims(f, clock))
	f.SetUnavailable(true)
	mustReject(t, verifier(t, f, clock), token)
	f.SetUnavailable(false)
	v, err := New(context.Background(), f.Server.URL, testAudience, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustReject(t, v, token)
}

func TestConcurrentVerificationsShareKeyFetch(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	token := sign(t, f, validClaims(f, clock))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := v.Verify(context.Background(), token); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := f.Requests(); got != 1 {
		t.Fatalf("concurrent verification made %d requests, want 1", got)
	}
}

func TestCanceledVerification(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	clock := newTestClock()
	v := verifier(t, f, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	principal, err := v.Verify(ctx, sign(t, f, validClaims(f, clock)))
	if !errors.Is(err, context.Canceled) || principal != (Principal{}) {
		t.Fatalf("canceled verification = %+v, %v", principal, err)
	}
	if f.Requests() != 0 {
		t.Fatal("canceled verification fetched keys")
	}
	var unconfigured Cloudflare
	mustReject(t, &unconfigured, "not-a-token")
}
