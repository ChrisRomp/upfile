// Package auth verifies administrator identities from signed Cloudflare Access
// assertions. Callers must supply the assertion itself, not identity headers.
//
// Cloudflare requires a subject, the configured issuer and audience, an RS256
// signature, and a future expiration. Not-before is enforced without leeway;
// optional issued-at claims permit at most one minute of forward clock skew.
// JWKS are cached for at most five minutes. Fetches have a five-second timeout
// and are limited to one every thirty seconds, with a startup burst of two.
// Unknown keys can therefore fail verification until the refresh cooldown ends.
// Expired cached keys are not used during an outage.
package auth

import "context"

// Principal is an identity established by a verified token. Subject is the stable
// identifier; Email is optional and is never populated from an HTTP header.
type Principal struct {
	Subject string `json:"subject"`
	Email   string `json:"email"`
}

// Verifier permits future authentication providers without changing callers.
type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}
