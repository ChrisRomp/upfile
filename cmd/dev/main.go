// Command dev runs the real application with local HTTPS and a development-only
// Access issuer. It must never be deployed or bound to a non-loopback interface.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"upfile/internal/app"
	"upfile/internal/auth"
	"upfile/web"
)

const (
	publicOrigin = "https://localhost:8443"
	adminOrigin  = "https://localhost:8444"
	devDir       = ".dev"
)

func run(ctx context.Context) error {
	publicListener, err := net.Listen("tcp4", "127.0.0.1:8443")
	if err != nil {
		return fmt.Errorf("listen for public HTTPS: %w", err)
	}
	defer publicListener.Close()
	adminListener, err := net.Listen("tcp4", "127.0.0.1:8444")
	if err != nil {
		return fmt.Errorf("listen for admin HTTPS: %w", err)
	}
	defer adminListener.Close()

	certificate, caPEM, err := localCertificate()
	if err != nil {
		return fmt.Errorf("create local HTTPS certificates: %w", err)
	}
	issuer, err := newLocalIssuer()
	if err != nil {
		return fmt.Errorf("create local Access issuer: %w", err)
	}
	defer issuer.server.Close()

	// Keep JWKS requests alive while in-flight admin requests drain on shutdown.
	verifierContext, cancelVerifier := context.WithCancel(context.Background())
	defer cancelVerifier()
	verifier, err := auth.New(verifierContext, issuer.server.URL, devAudience, issuer.server.Client())
	if err != nil {
		return err
	}
	a, err := app.New(app.Config{
		DataDir:      filepath.Join(devDir, "data"),
		PublicOrigin: publicOrigin,
		AdminOrigin:  adminOrigin,
	}, verifier, web.Assets())
	if err != nil {
		return err
	}
	defer a.Close()
	if err := writeCA(devDir, caPEM); err != nil {
		return fmt.Errorf("write development CA: %w", err)
	}

	public := localServer(a.Public(), certificate)
	admin := localServer(issuer.authenticate(a.Admin()), certificate)
	slog.Info("development HTTPS ready", "public", publicOrigin, "admin", adminOrigin, "issuer", issuer.server.URL)
	slog.Warn("LOCAL DEVELOPMENT ONLY: every admin request is signed as dev-admin; do not expose these listeners through a proxy or tunnel")
	slog.Info("Trust .dev/ca.pem in your browser, or accept the localhost certificate warning on BOTH ports; this CA changes on every restart. OS trust is never modified.")
	slog.Info("Private keys exist only in memory. Data persists in .dev/data; configure storage limits in the admin UI before uploading.")
	return serve(ctx, public, publicListener, admin, adminListener)
}

func localServer(handler http.Handler, certificate tls.Certificate) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		},
	}
}

func serve(ctx context.Context, public *http.Server, publicListener net.Listener, admin *http.Server, adminListener net.Listener) error {
	results := make(chan error, 2)
	go func() { results <- public.ServeTLS(publicListener, "", "") }()
	go func() { results <- admin.ServeTLS(adminListener, "", "") }()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-results:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, server := range []*http.Server{public, admin} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Shutdown(shutdown); err != nil {
				slog.Warn("forcing development listener shutdown", "error", err)
				_ = server.Close()
			}
		}()
	}
	wg.Wait()
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("development server stopped", "error", err)
		os.Exit(1)
	}
}
