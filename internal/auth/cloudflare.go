package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	maxTokenBytes = 16 * 1024
	issuedAtSkew  = time.Minute
)

// Cloudflare verifies RS256 Access assertions against a configured team origin.
// Reuse an instance across requests to share its bounded JWKS cache.
type Cloudflare struct {
	verifier *oidc.IDTokenVerifier
	issuer   string
	now      func() time.Time
}

var _ Verifier = (*Cloudflare)(nil)

// New validates the configured HTTPS team origin and application audience.
// JWKS are fetched lazily, exclusively from that origin's /cdn-cgi/access/certs.
// A supplied client can provide a trusted local fixture CA; TLS verification is
// never disabled. Its timeout is capped and redirects are not followed.
func New(ctx context.Context, issuer, audience string, client *http.Client) (*Cloudflare, error) {
	return newCloudflare(ctx, issuer, audience, client, time.Now)
}

func newCloudflare(ctx context.Context, issuer, audience string, client *http.Client, now func() time.Time) (*Cloudflare, error) {
	if ctx == nil {
		return nil, errors.New("cloudflare: context is required")
	}
	if strings.TrimSpace(audience) == "" || audience != strings.TrimSpace(audience) {
		return nil, errors.New("cloudflare: a nonempty application audience without surrounding whitespace is required")
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: invalid issuer URL: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || strings.Contains(issuer, "#") ||
		strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("cloudflare: issuer must be an HTTPS origin without credentials, query, fragment, or path")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("cloudflare: issuer port must be between 1 and 65535")
		}
	}
	issuer = strings.TrimSuffix(issuer, "/")
	jwksURL := issuer + "/cdn-cgi/access/certs"
	httpClient := newJWKSClient(client, jwksURL, now)
	keys := &expiringKeySet{
		now: now,
		newRemote: func() *oidc.RemoteKeySet {
			return oidc.NewRemoteKeySet(oidc.ClientContext(ctx, httpClient), jwksURL)
		},
	}
	return &Cloudflare{
		issuer: issuer,
		now:    now,
		verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: []string{oidc.RS256},
			Now:                  now,
		}),
	}, nil
}

// Verify returns only cryptographically verified claims. An invalid assertion
// always returns an empty Principal. Errors are intended for internal handling,
// not verbatim HTTP responses.
func (c *Cloudflare) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if c == nil || c.verifier == nil {
		return Principal{}, errors.New("cloudflare: verifier is not configured")
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, fmt.Errorf("cloudflare: verification canceled: %w", err)
	}
	if rawToken == "" || len(rawToken) > maxTokenBytes || strings.Count(rawToken, ".") != 2 {
		return Principal{}, errors.New("cloudflare: expected a bounded, compact signed JWT")
	}
	token, err := c.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("cloudflare: invalid assertion: %w", err)
	}
	// Keep issuer comparison exact, including for issuers with library-specific
	// compatibility exceptions. No assertion can select a different key source.
	if token.Issuer != c.issuer {
		return Principal{}, errors.New("cloudflare: assertion issuer does not match configuration")
	}
	var claims struct {
		Email     string   `json:"email"`
		ExpiresAt *float64 `json:"exp"`
		NotBefore *float64 `json:"nbf"`
		IssuedAt  *float64 `json:"iat"`
	}
	if err := token.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("cloudflare: invalid identity or temporal claims: %w", err)
	}
	now := c.now()
	nowSeconds := float64(now.Unix()) + float64(now.Nanosecond())/float64(time.Second)
	if claims.ExpiresAt == nil || *claims.ExpiresAt <= nowSeconds {
		return Principal{}, errors.New("cloudflare: assertion expiration is missing or expired")
	}
	// go-oidc allows five minutes of nbf skew; Access authorization uses the
	// actual not-before boundary instead.
	if claims.NotBefore != nil && *claims.NotBefore > nowSeconds {
		return Principal{}, errors.New("cloudflare: assertion is not yet valid")
	}
	if claims.IssuedAt != nil && (*claims.IssuedAt > nowSeconds+issuedAtSkew.Seconds() ||
		*claims.IssuedAt >= *claims.ExpiresAt) {
		return Principal{}, errors.New("cloudflare: assertion issued-at time is invalid or too far in the future")
	}
	if strings.TrimSpace(token.Subject) == "" {
		return Principal{}, errors.New("cloudflare: assertion subject is required")
	}
	return Principal{Subject: token.Subject, Email: claims.Email}, nil
}
