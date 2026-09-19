package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"upfile/internal/app"
	"upfile/internal/auth"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(".", ".dev-test-"+rand.Text())
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestCertificatesAndTrustFile(t *testing.T) {
	certificate, caPEM, err := localCertificate()
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("expected only a public CA certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Fatal("trust certificate is not a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if _, err := certificate.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Fatalf("certificate does not verify for %s: %v", host, err)
		}
	}
	if _, err := certificate.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com"}); err == nil {
		t.Fatal("certificate unexpectedly covers a non-local host")
	}
	_, secondCA, err := localCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if string(secondCA) == string(caPEM) {
		t.Fatal("CA was reused")
	}
	dir := testDirectory(t)
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("old certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCA(dir, caPEM); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("CA permissions are %o; want 0600", info.Mode().Perm())
	}
	written, err := os.ReadFile(caPath)
	if err != nil || string(written) != string(caPEM) {
		t.Fatalf("incorrect trust file: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected certificate files: %v, %v", entries, err)
	}
}

func TestIssuerWithRealVerifier(t *testing.T) {
	issuer, err := newLocalIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.server.Close()
	issuerURL, err := url.Parse(issuer.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(issuerURL.Hostname()); ip == nil || !ip.IsLoopback() || issuerURL.Scheme != "https" {
		t.Fatalf("issuer is not loopback HTTPS: %s", issuer.server.URL)
	}
	client := issuer.server.Client()
	response, err := client.Get(issuer.server.URL + "/cdn-cgi/access/certs")
	if err != nil {
		t.Fatal(err)
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	err = json.NewDecoder(response.Body).Decode(&jwks)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || len(jwks.Keys) != 1 {
		t.Fatalf("invalid JWKS: status=%d keys=%v error=%v", response.StatusCode, jwks.Keys, err)
	}
	for _, privateField := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		if _, ok := jwks.Keys[0][privateField]; ok {
			t.Fatalf("JWKS contains private key field %s", privateField)
		}
	}
	verifier, err := auth.New(context.Background(), issuer.server.URL, devAudience, client)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token, err := issuer.assertion(now)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "dev-admin" || principal.Email != "dev@localhost.invalid" {
		t.Fatalf("unexpected verified identity: %+v", principal)
	}
	t.Run("tampered signature", func(t *testing.T) {
		parts := strings.Split(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		signature[0] ^= 1
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)
		if _, err := verifier.Verify(context.Background(), strings.Join(parts, ".")); err == nil {
			t.Fatal("tampered signature was accepted")
		}
	})
	for _, test := range []struct {
		name string
		now  time.Time
	}{
		{"expired", now.Add(-2 * time.Hour)},
		{"future", now.Add(time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := issuer.assertion(test.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := verifier.Verify(context.Background(), token); err == nil {
				t.Fatal("invalid token time was accepted")
			}
		})
	}
	t.Run("wrong audience", func(t *testing.T) {
		wrongAudience, err := auth.New(context.Background(), issuer.server.URL, "not-upfile-dev", client)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wrongAudience.Verify(context.Background(), token); err == nil {
			t.Fatal("wrong audience was accepted")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		wrongIssuer, err := auth.New(context.Background(), "https://localhost:"+issuerURL.Port(), devAudience, client)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wrongIssuer.Verify(context.Background(), token); err == nil {
			t.Fatal("wrong issuer was accepted")
		}
	})
}

func TestAdminUsesRealAuthenticationAndSharedOrigin(t *testing.T) {
	issuer, err := newLocalIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.server.Close()
	verifier, err := auth.New(context.Background(), issuer.server.URL, devAudience, issuer.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(app.Config{
		DataDir: testDirectory(t),
		Origin:  devOrigin,
	}, verifier, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("dev app")}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	unauthenticated := httptest.NewRecorder()
	a.Admin().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, devOrigin+"/admin/api/settings", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("real admin handler accepted missing assertion: %d", unauthenticated.Code)
	}
	admin := issuer.authenticate(a.Admin())
	request := httptest.NewRequest(http.MethodGet, devOrigin+"/admin/api/settings", nil)
	request.Header.Set("Cf-Access-Jwt-Assertion", "attacker-supplied-token")
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("signed admin request failed: %d %s", response.Code, response.Body.String())
	}
	if request.Header.Get("Cf-Access-Jwt-Assertion") != "attacker-supplied-token" {
		t.Fatal("wrapper mutated the incoming request")
	}
	var settings struct {
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Configured {
		t.Fatal("harness preconfigured storage limits")
	}
	for _, test := range []struct {
		name   string
		origin string
		status int
	}{
		{"IP alias rejected", "https://127.0.0.1:8443", http.StatusForbidden},
		{"missing origin rejected", "", http.StatusForbidden},
		{"configured origin accepted", devOrigin, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"max_file_bytes":1048576,"storage_budget_bytes":10485760,"default_link_hours":24}`
			request := httptest.NewRequest(http.MethodPut, devOrigin+"/admin/api/settings", strings.NewReader(body))
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-Upfile-Request", "1")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			admin.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestHTTPSListenersAndGracefulShutdown(t *testing.T) {
	certificate, caPEM, err := localCertificate()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("could not trust local CA")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	issuer, err := newLocalIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.server.Close()
	verifier, err := auth.New(context.Background(), issuer.server.URL, devAudience, issuer.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Keep the real URL, SNI, and HTTP origin while avoiding fixed test ports.
			if address == "localhost:8443" {
				address = listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.ServerName != "localhost" {
			t.Error("handler did not receive localhost TLS")
		}
		if r.Host != strings.TrimPrefix(devOrigin, "https://") {
			t.Errorf("host changed: %q", r.Host)
		}
		admin := strings.HasPrefix(r.URL.Path, "/admin/")
		if admin {
			principal, err := verifier.Verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion"))
			if err != nil || principal.Subject != "dev-admin" {
				t.Errorf("stream authentication failed: principal=%+v err=%v", principal, err)
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
		} else if r.Header.Get("Cf-Access-Jwt-Assertion") != "" {
			t.Error("public request received an admin assertion")
		}
		if r.Method == http.MethodPatch {
			if r.Header.Get("Origin") != devOrigin {
				t.Errorf("origin changed: %q", r.Header.Get("Origin"))
			}
			if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
				t.Error("stream content type changed")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("stream read failed: %v", err)
				http.Error(w, "read failed", http.StatusBadRequest)
				return
			}
			if _, err := w.Write(body); err != nil {
				t.Errorf("stream response failed: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	public := handler
	admin := issuer.authenticate(handler)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			admin.ServeHTTP(w, r)
			return
		}
		public.ServeHTTP(w, r)
	})
	server := localServer(dispatch, certificate)
	server.ErrorLog = log.New(io.Discard, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- serve(ctx, server, listener) }()
	for _, endpoint := range []string{
		devOrigin + "/",
		devOrigin + "/admin/",
	} {
		response, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("HTTPS request failed: %d", response.StatusCode)
		}
		payload := bytes.Repeat([]byte{0, 1, 127, 128, 255}, 64<<10)
		request, err := http.NewRequest(http.MethodPatch, endpoint+"stream", io.NopCloser(bytes.NewReader(payload)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", devOrigin)
		request.Header.Set("Content-Type", "application/offset+octet-stream")
		response, err = client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		received, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Equal(payload, received) {
			t.Fatalf("TLS byte stream changed: status=%d bytes=%d err=%v", response.StatusCode, len(received), readErr)
		}
	}
	response, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("plaintext request was not rejected: %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listeners did not shut down")
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener remained open after shutdown")
	}
}
