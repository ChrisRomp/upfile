package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"time"
)

const devAudience = "upfile-dev"

type localIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newLocalIssuer() (*localIssuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(publicDER)
	issuer := &localIssuer{key: key, kid: base64.RawURLEncoding.EncodeToString(fingerprint[:])}
	jwks, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": issuer.kid,
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}},
	})
	if err != nil {
		return nil, err
	}
	certificate, _, err := localCertificate()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cdn-cgi/access/certs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write(jwks)
	})
	server := httptest.NewUnstartedServer(mux)
	// Supplying our own certificate avoids httptest's shared, static TLS key.
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.Config.IdleTimeout = 90 * time.Second
	server.Config.MaxHeaderBytes = 32 << 10
	server.StartTLS()
	issuer.server = server
	return issuer, nil
}

func (i *localIssuer) assertion(now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": i.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"iss":   i.server.URL,
		"sub":   "dev-admin",
		"email": "dev@localhost.invalid",
		"aud":   []string{devAudience},
		"type":  "app",
		"iat":   now.Add(-30 * time.Second).Unix(),
		"nbf":   now.Add(-30 * time.Second).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign development assertion: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (i *localIssuer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertion, err := i.assertion(time.Now())
		if err != nil {
			http.Error(w, "Could not create development assertion.", http.StatusInternalServerError)
			return
		}
		// Preserve Host, Origin, port, and TLS for the real app's origin checks.
		request := r.Clone(r.Context())
		request.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		next.ServeHTTP(w, request)
	})
}
